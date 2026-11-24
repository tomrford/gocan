//go:build windows && amd64

package vector

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/pcan"
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
				Bitrate: 500_000,
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
			FDTiming:     vectorFDTiming(dataBitrate),
		},
		openPeer: func(capture *gocan.Capture) (gocan.Bus, error) {
			return Open(context.Background(), capture, Config{
				ID:           2,
				Name:         "vector-fd-recovery-peer",
				ChannelIndex: vectorB,
				FDTiming:     vectorFDTiming(dataBitrate),
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
		FDTiming: vectorFDTiming(dataBitrate),
	})
	if err != nil {
		t.Fatalf("open Vector FD sender: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	receiver, err := Open(context.Background(), capture, Config{
		ID: 2, Name: "vector-fd-mismatch-receiver", ChannelIndex: vectorB,
		FDTiming: vectorFDTiming(mismatchedDataBitrate),
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
	waitForVectorEvents(t, capture, sender.ID(), cursor, func(event gocan.Event) bool {
		return event.Kind == gocan.EventErrorFrame
	})
	select {
	case <-receiver.Context().Done():
		t.Fatalf("Vector receiver stopped after an FD receive error: %v", context.Cause(receiver.Context()))
	default:
	}
}

// Two nodes that start the same identifier together collide on the first data
// bit where they differ. The node sending recessive there takes a bit error on
// every collision, so its transmit error counter keeps rising after both nodes
// are error-passive, and it goes bus-off. Error-passive retransmissions can
// drift apart and let a pair through uncontested, so both nodes queue enough
// pairs for collisions to resume. Mismatched timing cannot reach bus-off with
// two nodes: once the receiver is error-passive the sender sees only missing
// acknowledgements, which leave an error-passive transmitter's counter
// unchanged.
func TestVectorFDCollisionBusOffEvents(t *testing.T) {
	vectorA, vectorB := vectorPairIndexes(t)
	dataBitrate := vectorFDDataBitrate(t)

	capture := gocan.NewCapture()
	loser, err := Open(context.Background(), capture, Config{
		ID: 1, Name: "vector-fd-collision-loser", ChannelIndex: vectorA,
		FDTiming: vectorFDTiming(dataBitrate),
	})
	if err != nil {
		t.Fatalf("open Vector FD loser: %v", err)
	}
	t.Cleanup(func() { _ = loser.Close() })
	winner, err := Open(context.Background(), capture, Config{
		ID: 2, Name: "vector-fd-collision-winner", ChannelIndex: vectorB,
		FDTiming: vectorFDTiming(dataBitrate),
	})
	if err != nil {
		t.Fatalf("open Vector FD winner: %v", err)
	}
	t.Cleanup(func() { _ = winner.Close() })

	// A 64-byte frame without bit-rate switching holds the bus for over a
	// millisecond, so the first colliding pair queues behind it and starts
	// together after its interframe space.
	blocker, err := vectorEventFrame(0x5a6, 64, 0, gocan.FrameFD)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	winnerFrame, err := vectorEventFrame(0x5a7, 12, 0x00, gocan.FrameFD|gocan.FrameBitRateSwitch)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	loserFrame, err := vectorEventFrame(0x5a7, 12, 0x80, gocan.FrameFD|gocan.FrameBitRateSwitch)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := winner.Send(context.Background(), blocker); err != nil {
		t.Fatalf("send blocker: %v", err)
	}
	for range 32 {
		if err := winner.Send(context.Background(), winnerFrame); err != nil {
			t.Fatalf("send from collision winner: %v", err)
		}
		if err := loser.Send(context.Background(), loserFrame); err != nil &&
			!errors.Is(err, gocan.ErrTransmitQueueFull) && !errors.Is(err, gocan.ErrBusOff) {
			t.Fatalf("send from collision loser: %v", err)
		}
	}

	// Vector hardware rejoins the bus by itself after bus-off; the bus stays
	// open and reports both transitions.
	busOff := waitVectorState(t, capture, loser.ID(), 0, gocan.ControllerBusOff)
	active := waitVectorState(t, capture, loser.ID(), busOff, gocan.ControllerActive)
	for _, bus := range []gocan.Bus{loser, winner} {
		select {
		case <-bus.Context().Done():
			t.Fatalf("%s stopped: %v", bus.Name(), context.Cause(bus.Context()))
		default:
		}
	}
	loserSeq, _, _ := capture.BusEvents(loser.ID(), gocan.Range{})
	loserEvents := slices.Collect(loserSeq)
	if !slices.ContainsFunc(loserEvents[:busOff], func(event gocan.Event) bool { return event.Kind == gocan.EventErrorFrame }) {
		t.Fatalf("Vector bus-off event preceded its FD error event: %+v", loserEvents[:active+1])
	}

	cursor := capture.End()
	recovered, err := vectorEventFrame(0x5a8, 8, 0, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := gocan.Send(context.Background(), loser, recovered, 5*time.Second); err != nil {
		t.Fatalf("send after Vector bus-off recovery: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := capture.Next(ctx, gocan.FrameKey{Bus: winner.ID(), ID: recovered.ID, Direction: gocan.DirectionReceive}, cursor); err != nil {
		t.Fatalf("wait for frame sent after recovery: %v", err)
	}
}

// waitVectorState returns the index, within bus's events, of the first
// controller-state event at or after from that reports state.
func waitVectorState(t *testing.T, capture *gocan.Capture, bus gocan.BusID, from int, state gocan.ControllerState) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		eventSeq, _, _ := capture.BusEvents(bus, gocan.Range{})
		events := slices.Collect(eventSeq)
		for index := from; index < len(events); index++ {
			if events[index].Kind == gocan.EventControllerState && events[index].ControllerState == state {
				return index
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Vector bus %d reported no %v state: %+v", bus, state, events)
		}
		time.Sleep(5 * time.Millisecond)
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
	case <-vectorBus.Context().Done():
		t.Fatalf("Vector bus stopped in error-passive: %v", context.Cause(vectorBus.Context()))
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
	t.Fatalf("timed out waiting for Vector event; captured tail: %+v", events)
	return gocan.Cursor{}
}
