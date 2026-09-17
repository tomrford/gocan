//go:build windows

package pcan

import (
	"context"
	"errors"
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
	if got := len(capture.Series(key)); got != accepted {
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
	case <-target.Done():
		t.Fatalf("PCAN bus stopped in error-passive: %v", target.Err())
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
		events, next, err := capture.BusEventsSince(bus, cursor)
		if err != nil {
			t.Fatalf("BusEventsSince: %v", err)
		}
		for _, event := range events {
			if accept(event) {
				return next
			}
		}
		cursor = next
		time.Sleep(5 * time.Millisecond)
	}
	events := capture.BusEvents(bus)
	if len(events) > 10 {
		events = events[len(events)-10:]
	}
	t.Fatalf("timed out waiting for PCAN event; captured tail: %+v", events)
	return gocan.Cursor{}
}
