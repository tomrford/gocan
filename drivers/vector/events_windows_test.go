//go:build windows && amd64

package vector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/pcan"
)

func TestSameChipState(t *testing.T) {
	first, err := gocan.NewControllerStateEvent(
		1,
		time.Unix(1, 0),
		gocan.ControllerActive,
		0,
		0,
		true,
	)
	if err != nil {
		t.Fatalf("NewControllerStateEvent: %v", err)
	}
	second := first
	second.Timestamp = time.Unix(2, 0)
	if !sameChipState(first, second) {
		t.Fatal("identical controller state with a later observation time was not deduplicated")
	}
	second.TXErrorCount = 1
	if sameChipState(first, second) {
		t.Fatal("controller state with a changed error counter was deduplicated")
	}
}

func TestVectorClassicEvents(t *testing.T) {
	vectorIndex := vectorChannelIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX")
	pcanChannel := pcanPeerChannel(t, "GOCAN_PCAN_CHANNEL")

	runVectorEventRecovery(t, vectorEventSetup{
		target: Config{
			ID:           1,
			Name:         "vector-classic-event-target",
			ChannelIndex: vectorIndex,
			Bitrate:      500_000,
		},
		openPeer: func(capture *gocan.Capture) (gocan.Bus, error) {
			return pcan.Open(context.Background(), capture, pcan.Config{
				ID:      2,
				Name:    "pcan-recovery-peer",
				Channel: pcanChannel,
				Bitrate: pcan.Bitrate500K,
			})
		},
		faultID:     0x5a1,
		recoveryID:  0x5a2,
		payloadSize: 8,
	})
}

func TestVectorFDEvents(t *testing.T) {
	vectorA, vectorB := vectorPairIndexes(t)
	dataBitrate := vectorFDDataBitrate(t)

	runVectorEventRecovery(t, vectorEventSetup{
		target: Config{
			ID:           1,
			Name:         "vector-fd-event-target",
			ChannelIndex: vectorA,
			Bitrate:      500_000,
			DataBitrate:  dataBitrate,
		},
		openPeer: func(capture *gocan.Capture) (gocan.Bus, error) {
			return Open(context.Background(), capture, Config{
				ID:           2,
				Name:         "vector-fd-recovery-peer",
				ChannelIndex: vectorB,
				Bitrate:      500_000,
				DataBitrate:  dataBitrate,
			})
		},
		faultID:     0x5a4,
		recoveryID:  0x5a5,
		payloadSize: 12,
		flags:       gocan.FrameFD | gocan.FrameBitRateSwitch,
	})
}

func TestVectorFDMismatchedDataBitrateEvents(t *testing.T) {
	vectorA, vectorB := vectorPairIndexes(t)
	dataBitrate := vectorFDDataBitrate(t)
	mismatchedDataBitrate := dataBitrate / 2
	if mismatchedDataBitrate == 0 || mismatchedDataBitrate == dataBitrate {
		t.Skipf("cannot derive a mismatched data bitrate from %d", dataBitrate)
	}

	capture := gocan.NewCapture()
	sender, err := Open(context.Background(), capture, Config{
		ID: 1, Name: "vector-fd-mismatch-sender", ChannelIndex: vectorA,
		Bitrate: 500_000, DataBitrate: dataBitrate,
	})
	if err != nil {
		t.Fatalf("open Vector FD sender: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	receiver, err := Open(context.Background(), capture, Config{
		ID: 2, Name: "vector-fd-mismatch-receiver", ChannelIndex: vectorB,
		Bitrate: 500_000, DataBitrate: mismatchedDataBitrate,
	})
	if err != nil {
		t.Fatalf("open Vector FD receiver: %v", err)
	}
	t.Cleanup(func() { _ = receiver.Close() })

	cursor := capture.End()
	frame, err := vectorEventFrame(
		0x5a7,
		12,
		1,
		gocan.FrameFD|gocan.FrameBitRateSwitch,
	)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := sender.Send(context.Background(), frame); err != nil {
		t.Fatalf("send with mismatched data-phase timing: %v", err)
	}

	waitForVectorEvents(t, capture, receiver.ID(), cursor, func(event gocan.Event) bool {
		return event.Kind == gocan.EventErrorFrame
	})
	select {
	case <-receiver.Done():
		t.Fatalf("Vector receiver stopped after an FD receive error: %v", receiver.Err())
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-sender.Done():
	case <-ctx.Done():
		t.Fatal("Vector sender did not enter bus-off with mismatched FD data timing")
	}
	if !errors.Is(sender.Err(), gocan.ErrBusOff) {
		t.Fatalf("Vector sender Err() = %v, want ErrBusOff", sender.Err())
	}

	senderEvents := capture.BusEvents(sender.ID())
	seenError := false
	seenBusOff := false
	for _, event := range senderEvents {
		switch event.Kind {
		case gocan.EventErrorFrame:
			seenError = true
		case gocan.EventControllerState:
			if event.ControllerState == gocan.ControllerBusOff {
				if !seenError {
					t.Fatal("Vector bus-off event preceded its FD error event")
				}
				seenBusOff = true
			}
		}
	}
	if !seenError || !seenBusOff {
		t.Fatalf("Vector sender events omit error or bus-off: %+v", senderEvents)
	}
	lastEvent := senderEvents[len(senderEvents)-1]
	if lastEvent.Kind != gocan.EventControllerState ||
		lastEvent.ControllerState != gocan.ControllerBusOff {
		t.Fatalf("last Vector sender event = %+v, want bus-off", lastEvent)
	}
}

type vectorEventSetup struct {
	target      Config
	openPeer    func(*gocan.Capture) (gocan.Bus, error)
	faultID     uint32
	recoveryID  uint32
	payloadSize int
	flags       gocan.FrameFlags
}

func runVectorEventRecovery(t *testing.T, setup vectorEventSetup) {
	t.Helper()
	capture := gocan.NewCapture()
	vectorBus, err := Open(context.Background(), capture, setup.target)
	if err != nil {
		t.Fatalf("Open Vector target: %v", err)
	}
	t.Cleanup(func() { _ = vectorBus.Close() })

	cursor := waitForVectorEvents(t, capture, vectorBus.ID(), gocan.Cursor{}, func(event gocan.Event) bool {
		return event.Kind == gocan.EventControllerState &&
			event.ControllerState == gocan.ControllerActive
	})

	frame, err := vectorEventFrame(setup.faultID, setup.payloadSize, 1, setup.flags)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := vectorBus.Send(context.Background(), frame); err != nil {
		t.Fatalf("send without an active peer: %v", err)
	}

	seenError := false
	seenWarning := false
	cursor = waitForVectorEvents(t, capture, vectorBus.ID(), cursor, func(event gocan.Event) bool {
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
	case <-vectorBus.Done():
		t.Fatalf("Vector bus stopped in error-passive: %v", vectorBus.Err())
	default:
	}

	peer, err := setup.openPeer(capture)
	if err != nil {
		t.Fatalf("Open PCAN recovery peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	recoveryCursor := capture.End()
	recoveryFrame, err := vectorEventFrame(setup.recoveryID, setup.payloadSize, 9, setup.flags)
	if err != nil {
		t.Fatalf("NewFrame recovery: %v", err)
	}
	if err := peer.Send(context.Background(), recoveryFrame); err != nil {
		t.Fatalf("send recovery traffic: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       vectorBus.ID(),
		ID:        recoveryFrame.ID,
		Direction: gocan.DirectionReceive,
	}, recoveryCursor); err != nil {
		t.Fatalf("wait for Vector recovery traffic: %v", err)
	}
	for sequence := range 140 {
		frame, err := vectorEventFrame(setup.recoveryID+1, setup.payloadSize, byte(sequence), setup.flags)
		if err != nil {
			t.Fatalf("NewFrame recovery sequence %d: %v", sequence, err)
		}
		if err := vectorBus.Send(context.Background(), frame); err != nil {
			t.Fatalf("send Vector recovery sequence %d: %v", sequence, err)
		}
		time.Sleep(time.Millisecond)
	}
	waitForVectorEvents(t, capture, vectorBus.ID(), cursor, func(event gocan.Event) bool {
		return event.Kind == gocan.EventControllerState &&
			event.ControllerState == gocan.ControllerActive
	})
}

func vectorEventFrame(id uint32, payloadSize int, firstByte byte, flags gocan.FrameFlags) (gocan.Frame, error) {
	payload := make([]byte, payloadSize)
	payload[0] = firstByte
	return gocan.NewFrame(id, payload, flags)
}

func waitForVectorEvents(
	t *testing.T,
	capture *gocan.Capture,
	bus gocan.BusID,
	cursor gocan.Cursor,
	accept func(gocan.Event) bool,
) gocan.Cursor {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, next := capture.BusEventsSince(bus, cursor)
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
	t.Fatalf("timed out waiting for Vector event; captured tail: %+v", events)
	return gocan.Cursor{}
}
