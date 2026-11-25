//go:build windows

package pcan

import (
	"context"
	"errors"
	"os"
	"slices"
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
// B at 500 kbit/s while gocan.Send retries across recovery.
func TestPCANBusOffRecoveryHardware(t *testing.T) {
	if os.Getenv("GOCAN_PCAN_BUSOFF") != "1" {
		t.Skip("GOCAN_PCAN_BUSOFF is not set to 1")
	}
	channelA := testChannel(t, "GOCAN_PCAN_CHANNEL_A")
	channelB := testChannel(t, "GOCAN_PCAN_CHANNEL_B")

	capture := gocan.NewCapture()
	target, err := Open(context.Background(), capture, Config{
		ID: 1, Name: "pcan-busoff-target", Channel: channelA, Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("Open PCAN target: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	jammer, err := Open(context.Background(), capture, Config{
		ID: 2, Name: "pcan-busoff-jammer", Channel: channelB, Bitrate: 10_000,
	})
	if err != nil {
		t.Fatalf("Open PCAN 10k jammer: %v", err)
	}
	dominant, err := gocan.NewFrame(0x000, make([]byte, 8), 0)
	if err != nil {
		t.Fatalf("NewFrame dominant: %v", err)
	}
	jamCtx, stopJam := context.WithCancel(context.Background())
	jamDone := make(chan struct{})
	go func() {
		defer close(jamDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-jamCtx.Done():
				return
			case <-ticker.C:
			}
			_ = jammer.Send(context.Background(), dominant)
		}
	}()
	t.Cleanup(func() {
		stopJam()
		<-jamDone
		_ = jammer.Close()
	})

	faultFrame, err := gocan.NewFrame(0x7f1, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if err != nil {
		t.Fatalf("NewFrame fault: %v", err)
	}
	sawErrBusOff := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := target.Send(context.Background(), faultFrame)
		if errors.Is(err, gocan.ErrBusOff) {
			sawErrBusOff = true
			break
		}
		if err != nil && !errors.Is(err, gocan.ErrTransmitQueueFull) {
			t.Fatalf("PCAN target Send during bus-off fault = %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if !sawErrBusOff {
		t.Fatal("target Send did not return ErrBusOff")
	}
	waitForPCANEvents(t, capture, target.ID(), gocan.Cursor{}, func(event gocan.Event) bool {
		return event.Kind == gocan.EventControllerState && event.ControllerState == gocan.ControllerBusOff
	})
	select {
	case <-target.Context().Done():
		t.Fatalf("PCAN target stopped during bus-off: %v", context.Cause(target.Context()))
	default:
	}

	rejectedFrame, err := gocan.NewFrame(0x7f2, []byte{0x55}, 0)
	if err != nil {
		t.Fatalf("NewFrame rejected: %v", err)
	}
	if err := target.Send(context.Background(), rejectedFrame); !errors.Is(err, gocan.ErrBusOff) {
		t.Fatalf("PCAN Send while bus-off = %v, want ErrBusOff", err)
	}
	rejectedSeq, _, _ := capture.Series(gocan.FrameKey{
		Bus: target.ID(), ID: rejectedFrame.ID, Direction: gocan.DirectionTransmit,
	}, gocan.Range{})
	if got := len(slices.Collect(rejectedSeq)); got != 0 {
		t.Fatalf("ErrBusOff send recorded %d transmissions for %#x", got, rejectedFrame.ID)
	}

	// Start bounded recovery Send while still bus-off and wait for its first
	// ErrBusOff rejection before clearing the fault.
	recoveryFrame, err := gocan.NewFrame(0x7f3, []byte{0xAA}, 0)
	if err != nil {
		t.Fatalf("NewFrame recovery: %v", err)
	}
	rejectedOnce := make(chan struct{})
	observed := &pcanBusOffObserver{Bus: target, onBusOff: rejectedOnce}
	recovered := make(chan error, 1)
	go func() {
		recovered <- gocan.Send(context.Background(), observed, recoveryFrame, 5*time.Second)
	}()
	select {
	case <-rejectedOnce:
	case <-time.After(2 * time.Second):
		t.Fatal("gocan.Send did not hit ErrBusOff before fault removal")
	}
	recoveryCursor := capture.End()

	stopJam()
	<-jamDone
	if err := jammer.Close(); err != nil {
		t.Fatalf("Close PCAN jammer: %v", err)
	}
	peer, err := Open(context.Background(), capture, Config{
		ID: 3, Name: "pcan-busoff-peer", Channel: channelB, Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("Open PCAN recovery peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	if err := <-recovered; err != nil {
		t.Fatalf("gocan.Send across bus-off recovery: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus: peer.ID(), ID: recoveryFrame.ID, Direction: gocan.DirectionReceive,
	}, recoveryCursor); err != nil {
		t.Fatalf("peer did not receive recovery frame: %v", err)
	}
	// The 10 kbit/s jammer leaves the target with REC >= 128; per ISO 11898
	// REC only decreases on successful reception, so send clean peer traffic
	// before waiting for ControllerActive.
	cleanFrame, err := gocan.NewFrame(0x7f5, []byte{0x55}, 0)
	if err != nil {
		t.Fatalf("NewFrame clean: %v", err)
	}
	for i := range 64 {
		if err := gocan.Send(context.Background(), peer, cleanFrame, time.Second); err != nil {
			t.Fatalf("send clean peer frame %d: %v", i, err)
		}
	}
	waitForPCANEvents(t, capture, target.ID(), recoveryCursor, func(event gocan.Event) bool {
		return event.Kind == gocan.EventControllerState && event.ControllerState == gocan.ControllerActive
	})
	select {
	case <-target.Context().Done():
		t.Fatalf("PCAN target stopped after recovery: %v", context.Cause(target.Context()))
	default:
	}
}

type pcanBusOffObserver struct {
	gocan.Bus
	onBusOff chan struct{}
	notified bool
}

func (bus *pcanBusOffObserver) Send(ctx context.Context, frame gocan.Frame) error {
	err := bus.Bus.Send(ctx, frame)
	if !bus.notified && errors.Is(err, gocan.ErrBusOff) {
		bus.notified = true
		close(bus.onBusOff)
	}
	return err
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
