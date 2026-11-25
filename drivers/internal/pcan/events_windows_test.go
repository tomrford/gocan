//go:build windows

package pcan

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

func TestPCANTransmitQueueFull(t *testing.T) {
	channelA := testChannel(t, "GOCAN_PCAN_CHANNEL_A")
	channelB := testChannel(t, "GOCAN_PCAN_CHANNEL_B")
	capture := gocan.NewCapture()
	target, err := Open(context.Background(), capture, Config{
		ID: 1, Name: "pcan-target", Channel: channelA, Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("Open PCAN target: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })
	peer, err := Open(context.Background(), capture, Config{
		ID: 2, Name: "pcan-peer", Channel: channelB, Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("Open PCAN peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	frame, err := gocan.NewFrame(0x591, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	accepted := 0
	for range 100_000 {
		err := target.Send(context.Background(), frame)
		if err == nil {
			accepted++
			continue
		}
		if !errors.Is(err, gocan.ErrTransmitQueueFull) {
			t.Fatalf("saturated PCAN Send = %v, want ErrTransmitQueueFull", err)
		}
		break
	}
	if accepted == 100_000 {
		t.Fatal("PCAN transmit queue did not fill")
	}
	key := gocan.FrameKey{
		Bus:       target.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionTransmit,
	}
	seq, _, _ := capture.Series(key, gocan.Range{})
	if got := len(slices.Collect(seq)); got != accepted {
		t.Fatalf("capture holds %d transmissions after %d accepted sends and one rejection", got, accepted)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		err := target.Send(context.Background(), frame)
		if err == nil {
			break
		}
		if !errors.Is(err, gocan.ErrTransmitQueueFull) {
			t.Fatalf("PCAN Send while queue drains = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("PCAN transmit queue did not recover")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPCANClassicEvents(t *testing.T) {
	channelA := testChannel(t, "GOCAN_PCAN_CHANNEL_A")
	channelB := testChannel(t, "GOCAN_PCAN_CHANNEL_B")

	capture := gocan.NewCapture()
	target, err := Open(context.Background(), capture, Config{
		ID:      1,
		Name:    "pcan-event-target",
		Channel: channelA,
		Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("Open PCAN target: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	cursor := capture.End()
	frame, err := gocan.NewFrame(0x5b1, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := target.Send(context.Background(), frame); err != nil {
		t.Fatalf("send without an active peer: %v", err)
	}

	seenError := false
	seenWarning := false
	cursor = waitForPCANEvents(t, capture, target.ID(), cursor, func(event gocan.Event) bool {
		switch event.Kind {
		case gocan.EventErrorFrame:
			seenError = true
		case gocan.EventControllerState:
			if event.ControllerState == gocan.ControllerWarning {
				seenWarning = true
			}
			if event.ControllerState == gocan.ControllerPassive {
				if !event.ErrorCountsKnown || event.TXErrorCount < 128 {
					t.Fatalf("passive event has counters %+v", event)
				}
				return seenError && seenWarning
			}
		}
		return false
	})
	select {
	case <-target.Context().Done():
		t.Fatalf("PCAN bus stopped in error-passive: %v", context.Cause(target.Context()))
	default:
	}

	peer, err := Open(context.Background(), capture, Config{
		ID:      2,
		Name:    "pcan-recovery-peer",
		Channel: channelB,
		Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("Open PCAN recovery peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	for sequence := range 140 {
		frame, err := gocan.NewFrame(0x5b2, []byte{byte(sequence)}, 0)
		if err != nil {
			t.Fatalf("NewFrame recovery sequence %d: %v", sequence, err)
		}
		if err := target.Send(context.Background(), frame); err != nil {
			t.Fatalf("send recovery sequence %d: %v", sequence, err)
		}
		time.Sleep(time.Millisecond)
	}

	recoveryCursor := capture.End()
	recoveryFrame, err := gocan.NewFrame(0x5b3, []byte{9}, 0)
	if err != nil {
		t.Fatalf("NewFrame peer recovery: %v", err)
	}
	if err := peer.Send(context.Background(), recoveryFrame); err != nil {
		t.Fatalf("send peer recovery traffic: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       target.ID(),
		ID:        recoveryFrame.ID,
		Direction: gocan.DirectionReceive,
	}, recoveryCursor); err != nil {
		t.Fatalf("wait for PCAN recovery traffic: %v", err)
	}
	waitForPCANEvents(t, capture, target.ID(), cursor, func(event gocan.Event) bool {
		return event.Kind == gocan.EventControllerState &&
			event.ControllerState == gocan.ControllerActive
	})
}

// TestPCANBusOffRecoveryHardware forces a real bus-off on channel A by having
// channel B stream dominant all-zero frames at 10 kbit/s, then reopens channel
// B at 500 kbit/s and verifies recovery without closing A.
func TestPCANBusOffRecoveryHardware(t *testing.T) {
	if os.Getenv("GOCAN_PCAN_BUSOFF") != "1" {
		t.Skip("GOCAN_PCAN_BUSOFF is not set to 1")
	}
	channelA := testChannel(t, "GOCAN_PCAN_CHANNEL_A")
	channelB := testChannel(t, "GOCAN_PCAN_CHANNEL_B")

	capture := gocan.NewCapture()
	target, err := Open(context.Background(), capture, Config{
		ID:      1,
		Name:    "pcan-busoff-target",
		Channel: channelA,
		Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("Open PCAN target: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	jammer, err := Open(context.Background(), capture, Config{
		ID:      2,
		Name:    "pcan-busoff-jammer",
		Channel: channelB,
		Bitrate: 10_000,
	})
	if err != nil {
		t.Fatalf("Open PCAN 10k jammer: %v", err)
	}

	stopJam := make(chan struct{})
	var jamOnce sync.Once
	var jamWG sync.WaitGroup
	jamWG.Add(1)
	go func() {
		defer jamWG.Done()
		dominant, err := gocan.NewFrame(0x000, make([]byte, 8), 0)
		if err != nil {
			t.Errorf("NewFrame dominant: %v", err)
			return
		}
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopJam:
				return
			case <-ticker.C:
			}
			_ = jammer.Send(context.Background(), dominant)
		}
	}()
	stopAndCloseJammer := func() error {
		var closeErr error
		jamOnce.Do(func() {
			close(stopJam)
			jamWG.Wait()
			closeErr = jammer.Close()
		})
		return closeErr
	}
	t.Cleanup(func() { _ = stopAndCloseJammer() })

	faultFrame, err := gocan.NewFrame(0x7f1, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if err != nil {
		t.Fatalf("NewFrame fault: %v", err)
	}
	accepted := 0
	busOffRejections := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := target.Send(context.Background(), faultFrame)
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, gocan.ErrBusOff):
			busOffRejections++
		case errors.Is(err, gocan.ErrTransmitQueueFull):
		default:
			t.Fatalf("PCAN target Send during bus-off fault = %v", err)
		}
		if latest, ok := capture.LatestControllerState(target.ID()); ok &&
			latest.ControllerState == gocan.ControllerBusOff &&
			busOffRejections > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	latest, ok := capture.LatestControllerState(target.ID())
	if !ok || latest.ControllerState != gocan.ControllerBusOff {
		t.Fatalf("target LatestControllerState = %+v (ok=%v), want ControllerBusOff (busOffRejections=%d)", latest, ok, busOffRejections)
	}
	if busOffRejections == 0 {
		t.Fatal("target reached ControllerBusOff without Send returning ErrBusOff")
	}
	select {
	case <-target.Context().Done():
		t.Fatalf("PCAN target stopped during bus-off: %v", context.Cause(target.Context()))
	default:
	}

	faultSeq, _, _ := capture.Series(gocan.FrameKey{
		Bus:       target.ID(),
		ID:        faultFrame.ID,
		Direction: gocan.DirectionTransmit,
	}, gocan.Range{})
	if got := len(slices.Collect(faultSeq)); got != accepted {
		t.Fatalf("capture holds %d fault transmissions after %d accepted sends and %d ErrBusOff rejections", got, accepted, busOffRejections)
	}

	rejectedFrame, err := gocan.NewFrame(0x7f2, []byte{0x55}, 0)
	if err != nil {
		t.Fatalf("NewFrame rejected: %v", err)
	}
	if err := target.Send(context.Background(), rejectedFrame); errors.Is(err, gocan.ErrBusOff) {
		rejectedSeq, _, _ := capture.Series(gocan.FrameKey{
			Bus:       target.ID(),
			ID:        rejectedFrame.ID,
			Direction: gocan.DirectionTransmit,
		}, gocan.Range{})
		if got := len(slices.Collect(rejectedSeq)); got != 0 {
			t.Fatalf("Send returned ErrBusOff but recorded %d transmissions for %#x", got, rejectedFrame.ID)
		}
	} else if err != nil && !errors.Is(err, gocan.ErrTransmitQueueFull) {
		t.Fatalf("PCAN Send while bus-off = %v", err)
	}

	if err := stopAndCloseJammer(); err != nil {
		t.Fatalf("Close PCAN jammer: %v", err)
	}

	peer, err := Open(context.Background(), capture, Config{
		ID:      3,
		Name:    "pcan-busoff-peer",
		Channel: channelB,
		Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("Open PCAN recovery peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	recoveryFrame, err := gocan.NewFrame(0x7f3, []byte{0xAA}, 0)
	if err != nil {
		t.Fatalf("NewFrame recovery: %v", err)
	}
	cursor := capture.End()
	if err := gocan.Send(context.Background(), target, recoveryFrame, 3*time.Second); err != nil {
		t.Fatalf("gocan.Send across bus-off recovery: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       peer.ID(),
		ID:        recoveryFrame.ID,
		Direction: gocan.DirectionReceive,
	}, cursor); err != nil {
		t.Fatalf("peer did not receive recovery frame: %v", err)
	}
	recoverySeq, _, _ := capture.Series(gocan.FrameKey{
		Bus:       target.ID(),
		ID:        recoveryFrame.ID,
		Direction: gocan.DirectionTransmit,
	}, gocan.Range{})
	if got := len(slices.Collect(recoverySeq)); got != 1 {
		t.Fatalf("recovered Send recorded %d transmissions, want 1", got)
	}

	// Clean receptions bring target's receive error counter below warning so
	// recovery polling observes PCAN_ERROR_OK and reports ControllerActive.
	for i := range 64 {
		back, err := gocan.NewFrame(0x7f5, []byte{byte(i)}, 0)
		if err != nil {
			t.Fatalf("NewFrame peer traffic %d: %v", i, err)
		}
		if err := gocan.Send(context.Background(), peer, back, time.Second); err != nil {
			t.Fatalf("peer Send %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
	}
	waitPCANControllerState(t, capture, target.ID(), gocan.ControllerActive)
	select {
	case <-target.Context().Done():
		t.Fatalf("PCAN target stopped after recovery: %v", context.Cause(target.Context()))
	default:
	}
}

func waitPCANControllerState(t *testing.T, capture *gocan.Capture, bus gocan.BusID, want gocan.ControllerState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if latest, ok := capture.LatestControllerState(bus); ok && latest.ControllerState == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	latest, ok := capture.LatestControllerState(bus)
	t.Fatalf("timed out waiting for PCAN bus %d controller state %v; latest = %+v (ok=%v)", bus, want, latest, ok)
}

func waitForPCANEvents(
	t *testing.T,
	capture *gocan.Capture,
	bus gocan.BusID,
	cursor gocan.Cursor,
	accept func(gocan.Event) bool,
) gocan.Cursor {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, next, err := capture.BusEvents(bus, gocan.Since(cursor))
		if err != nil {
			t.Fatalf("BusEvents: %v", err)
		}
		for event := range events {
			if accept(event) {
				return next
			}
		}
		cursor = next
		time.Sleep(5 * time.Millisecond)
	}
	allEvents, _, _ := capture.BusEvents(bus, gocan.Range{})
	events := slices.Collect(allEvents)
	if len(events) > 10 {
		events = events[len(events)-10:]
	}
	t.Fatalf("timed out waiting for PCAN event; captured tail: %+v", events)
	return gocan.Cursor{}
}
