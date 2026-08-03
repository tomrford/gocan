package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

const benchDrainTimeout = 30 * time.Second

func receiveKey(receiver gocan.Bus, frame gocan.Frame) gocan.FrameKey {
	return gocan.FrameKey{
		Bus:       receiver.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionReceive,
		Extended:  frame.Flags.Has(gocan.FrameExtended),
	}
}

// RoundTripBenchmark measures one frame's full path per iteration: Send on
// sender, wire transmission, receiver's native reception, and the RX append
// becoming observable through Next. Adapter delivery latency dominates wire
// time on USB hardware; the software layers must stay negligible beside both.
func RoundTripBenchmark(b *testing.B, capture *gocan.Capture, sender, receiver gocan.Bus, frame gocan.Frame) {
	b.Helper()
	ctx := context.Background()
	key := receiveKey(receiver, frame)
	cursor := capture.End()

	b.ReportAllocs()
	for b.Loop() {
		if err := sender.Send(ctx, frame); err != nil {
			b.Fatalf("Send: %v", err)
		}
		_, next, err := capture.Next(ctx, key, cursor)
		if err != nil {
			b.Fatalf("Next: %v", err)
		}
		cursor = next
	}
}

// SaturatedCaptureBenchmark measures sustained full-bus-load capture: the
// sender keeps the native transmit queue full and, when it is, blocks on the
// next peer reception, so ns/op converges on true wire pacing. Reported
// allocations cover the whole process per frame: the send path, both drivers'
// receive loops, both capture appends, and the consuming reader.
func SaturatedCaptureBenchmark(b *testing.B, capture *gocan.Capture, sender, receiver gocan.Bus, frame gocan.Frame) {
	b.Helper()
	ctx := context.Background()
	key := receiveKey(receiver, frame)
	cursor := capture.End()
	received := 0
	sent := 0

	waitNext := func(waitCtx context.Context) {
		_, next, err := capture.Next(waitCtx, key, cursor)
		if err != nil {
			b.Fatalf("Next after %d of %d receptions: %v", received, sent, err)
		}
		cursor = next
		received++
	}

	// Fill the native transmit queue before timing so every timed iteration is
	// wire-paced; mixing cheap queue-accept iterations in breaks b.Loop's
	// duration estimation.
	const primeLimit = 100_000
	for {
		err := sender.Send(ctx, frame)
		if err == nil {
			if sent++; sent > primeLimit {
				b.Fatalf("transmit queue accepted %d frames without saturating", sent)
			}
			continue
		}
		if !errors.Is(err, gocan.ErrTransmitQueueFull) {
			b.Fatalf("Send while priming: %v", err)
		}
		break
	}

	b.ReportAllocs()
	for b.Loop() {
		// One reception frees exactly one wire slot for the next send.
		waitNext(ctx)
		for {
			err := sender.Send(ctx, frame)
			if err == nil {
				sent++
				break
			}
			if !errors.Is(err, gocan.ErrTransmitQueueFull) {
				b.Fatalf("Send: %v", err)
			}
			waitNext(ctx)
		}
	}

	drainCtx, cancel := context.WithTimeout(ctx, benchDrainTimeout)
	defer cancel()
	for received < sent {
		waitNext(drainCtx)
	}
}
