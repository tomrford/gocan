// Package conformance provides the driver-independent test suite that every
// Bus implementation must pass.
//
// A driver package hooks in with a single test:
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, openPair, conformance.Capabilities{FD: true})
//	}
//
// where openPair yields endpoints on one shared medium. The endpoints need
// not come from the same driver: a hardware runner may open a reference
// adapter on one side and the device under test on the other, wired together
// and terminated. Capabilities describe the intersection of the pair.
package conformance

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

// waitTimeout bounds every wait for captured traffic. It is generous so that
// hardware latency never fails a healthy driver.
const waitTimeout = 5 * time.Second

// Pair opens two connected endpoints on one shared medium, both contributing
// to capture. It is called once per test case; the pair must be fresh, and
// the opener is responsible for closing whatever the case does not close
// (register t.Cleanup). A medium that can only provide one endpoint returns
// nil for the second, which skips the cases that need a peer.
type Pair func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus)

// Capabilities describe what the opened pair supports: the intersection of
// both endpoints.
type Capabilities struct {
	// FD enables the CAN FD frame cases.
	FD bool
	// RemoteFrames enables the classical remote frame case.
	RemoteFrames bool
	// InduceOverrun forces a receive overrun on target while leaving healthy
	// usable. Both buses belong to this suite's medium. Nil skips the overrun
	// contract case: a healthy receive loop always drains, so most real media
	// cannot induce one on demand.
	InduceOverrun func(t *testing.T, healthy, target gocan.Bus)
}

// Run executes the conformance suite.
func Run(t *testing.T, open Pair, caps Capabilities) {
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, open) })
	t.Run("first frame after open", func(t *testing.T) { testFirstFrameAfterOpen(t, open) })
	t.Run("rejects invalid frames", func(t *testing.T) { testRejectsInvalid(t, open) })
	t.Run("cancellation is consistent", func(t *testing.T) { testCancellation(t, open) })
	t.Run("frame shapes", func(t *testing.T) { testFrameShapes(t, open, caps) })
	t.Run("concurrent traffic", func(t *testing.T) { testConcurrentTraffic(t, open) })
	t.Run("overrun stops the bus", func(t *testing.T) { testOverrun(t, open, caps) })
}

func openPair(t *testing.T, open Pair) (*gocan.Capture, gocan.Bus, gocan.Bus) {
	t.Helper()
	capture := gocan.NewCapture()
	a, b := open(t, capture)
	if a == nil {
		t.Fatal("medium returned no first endpoint")
	}
	if a.ID() == 0 || a.Name() == "" {
		t.Fatal("first endpoint has no ID or name")
	}
	if b != nil && a.ID() == b.ID() {
		t.Fatalf("endpoints share bus ID %d", a.ID())
	}
	if b != nil && (b.ID() == 0 || b.Name() == "") {
		t.Fatal("second endpoint has no ID or name")
	}
	return capture, a, b
}

func requirePeer(t *testing.T, b gocan.Bus) {
	t.Helper()
	if b == nil {
		t.Skip("medium provides a single endpoint")
	}
}

func frameKey(bus gocan.Bus, frame gocan.Frame, direction gocan.Direction) gocan.FrameKey {
	return gocan.FrameKey{
		Bus:       bus.ID(),
		ID:        frame.ID,
		Direction: direction,
		Extended:  frame.Flags.Has(gocan.FrameExtended),
	}
}

// nextEvent waits for the first frame matching key after cursor.
func nextEvent(t *testing.T, capture *gocan.Capture, key gocan.FrameKey, cursor gocan.Cursor) (gocan.FrameEvent, gocan.Cursor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	event, next, err := capture.Next(ctx, key, cursor)
	if err != nil {
		t.Fatalf("waiting for %+v: %v", key, err)
	}
	return event, next
}

func requireClosed(t *testing.T, bus gocan.Bus) {
	t.Helper()
	select {
	case <-bus.Done():
	case <-time.After(waitTimeout):
		t.Fatal("Done was not closed")
	}
}

func testLifecycle(t *testing.T, open Pair) {
	for cycle := range 3 {
		capture, a, b := openPair(t, open)

		select {
		case <-a.Done():
			t.Fatalf("cycle %d: bus reports done before Close", cycle)
		default:
		}

		frame, err := gocan.NewFrame(uint32(0x100+cycle), []byte{byte(cycle)}, 0)
		if err != nil {
			t.Fatalf("NewFrame: %v", err)
		}
		if err := a.Send(context.Background(), frame); err != nil {
			t.Fatalf("cycle %d: Send: %v", cycle, err)
		}
		event, _ := nextEvent(t, capture, frameKey(a, frame, gocan.DirectionTransmit), gocan.Cursor{})
		if event.Direction != gocan.DirectionTransmit || event.Frame != frame {
			t.Fatalf("cycle %d: captured %+v, want the accepted transmission", cycle, event)
		}
		if event.Timestamp.IsZero() {
			t.Fatalf("cycle %d: transmission carries no timestamp", cycle)
		}

		if err := a.Close(); err != nil {
			t.Fatalf("cycle %d: Close: %v", cycle, err)
		}
		requireClosed(t, a)
		if err := a.Err(); err != nil {
			t.Fatalf("cycle %d: Err after normal close = %v, want nil", cycle, err)
		}
		if err := a.Close(); err != nil {
			t.Fatalf("cycle %d: repeated Close: %v", cycle, err)
		}
		if err := a.Send(context.Background(), frame); !errors.Is(err, gocan.ErrBusClosed) {
			t.Fatalf("cycle %d: Send after close = %v, want ErrBusClosed", cycle, err)
		}

		if b != nil {
			// Closing one bus must not affect its peers or the capture.
			if err := b.Send(context.Background(), frame); err != nil {
				t.Fatalf("cycle %d: peer Send after close: %v", cycle, err)
			}
			nextEvent(t, capture, frameKey(b, frame, gocan.DirectionTransmit), gocan.Cursor{})
			if err := b.Close(); err != nil {
				t.Fatalf("cycle %d: peer Close: %v", cycle, err)
			}
		}
	}
}

// testFirstFrameAfterOpen verifies that a pair can carry traffic the moment
// Open returns. A driver whose channel is not ready when Open hands it back
// loses that frame outright rather than delaying it, so this case fails on any
// single loss.
//
// The loss it guards against is intermittent, which is why the case samples
// repeatedly instead of sending once: the PCAN driver lost roughly a quarter of
// first frames until Open began waiting for its channel, and a single sample
// would have passed three times in four. See channelSettleDelay in
// drivers/pcan. This is also the case to run when qualifying new hardware,
// because a settle budget that is too short for a slower host fails silently.
func testFirstFrameAfterOpen(t *testing.T, open Pair) {
	const samples = 20

	for sample := range samples {
		capture, a, b := openPair(t, open)
		requirePeer(t, b)

		// A distinct identifier per sample, so a frame arriving late cannot be
		// mistaken for a later sample's.
		frame, err := gocan.NewFrame(uint32(0x600+sample), []byte{byte(sample)}, 0)
		if err != nil {
			t.Fatalf("NewFrame: %v", err)
		}
		if err := a.Send(context.Background(), frame); err != nil {
			t.Fatalf("sample %d: Send immediately after Open: %v", sample, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		_, _, err = capture.Next(ctx, frameKey(b, frame, gocan.DirectionReceive), gocan.Cursor{})
		cancel()
		if err != nil {
			t.Fatalf("sample %d: %s never received the frame %s sent immediately after Open: %v",
				sample, b.Name(), a.Name(), err)
		}

		if err := a.Close(); err != nil {
			t.Fatalf("sample %d: Close: %v", sample, err)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("sample %d: peer Close: %v", sample, err)
		}
	}
}

func testRejectsInvalid(t *testing.T, open Pair) {
	capture, a, _ := openPair(t, open)

	invalid := []gocan.Frame{
		{ID: gocan.MaxStandardID + 1},
		{ID: 0x100, DLC: 16},
		{ID: 0x100, Flags: gocan.FrameBitRateSwitch},
	}
	for _, frame := range invalid {
		if err := a.Send(context.Background(), frame); err == nil {
			t.Fatalf("Send accepted invalid frame %+v", frame)
		}
	}
	if got := len(capture.Frames()); got != 0 {
		t.Fatalf("capture holds %d frames after rejected sends", got)
	}

	frame, err := gocan.NewFrame(0x100, []byte{1}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := a.Send(context.Background(), frame); err != nil {
		t.Fatalf("Send after rejections: %v", err)
	}
	nextEvent(t, capture, frameKey(a, frame, gocan.DirectionTransmit), gocan.Cursor{})
}

// testCancellation verifies that a cancelled Send reports a definite outcome:
// either the context error with nothing captured, or nil with the
// transmission captured. It must never lose a frame it reported sent, nor
// report failure for a frame it handed to the medium.
func testCancellation(t *testing.T, open Pair) {
	capture, a, _ := openPair(t, open)

	frame, err := gocan.NewFrame(0x123, []byte{1}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	switch err := a.Send(ctx, frame); {
	case err == nil:
		event, _ := nextEvent(t, capture, frameKey(a, frame, gocan.DirectionTransmit), gocan.Cursor{})
		if event.Direction != gocan.DirectionTransmit {
			t.Fatalf("captured %+v, want the accepted transmission", event)
		}
	case errors.Is(err, context.Canceled):
		if got := len(capture.Frames()); got != 0 {
			t.Fatalf("Send reported cancellation but the capture holds %d frames", got)
		}
	default:
		t.Fatalf("cancelled Send = %v, want nil or context.Canceled", err)
	}
}

func testFrameShapes(t *testing.T, open Pair, caps Capabilities) {
	type shape struct {
		name  string
		frame gocan.Frame
	}
	var shapes []shape
	add := func(name string, frame gocan.Frame, err error) {
		if err != nil {
			t.Fatalf("building %s: %v", name, err)
		}
		shapes = append(shapes, shape{name: name, frame: frame})
	}

	empty, err := gocan.NewFrame(0x101, nil, 0)
	add("standard empty", empty, err)
	classical, err := gocan.NewFrame(gocan.MaxStandardID, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	add("standard max ID", classical, err)
	extended, err := gocan.NewFrame(gocan.MaxExtendedID, []byte{9, 10}, gocan.FrameExtended)
	add("extended max ID", extended, err)
	if caps.FD {
		fd, err := gocan.NewFrame(0x102, make([]byte, 64), gocan.FrameFD)
		add("fd 64", fd, err)
		brs, err := gocan.NewFrame(0x103, make([]byte, 12), gocan.FrameFD|gocan.FrameBitRateSwitch)
		add("fd bit-rate switch", brs, err)
	}
	if caps.RemoteFrames {
		remote, err := gocan.NewRemoteFrame(0x104, 4, false)
		add("remote", remote, err)
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			capture, a, b := openPair(t, open)
			requirePeer(t, b)

			// Both directions: each endpoint transmits, the other receives.
			for _, side := range []struct{ from, to gocan.Bus }{{a, b}, {b, a}} {
				cursor := capture.End()
				if err := side.from.Send(context.Background(), shape.frame); err != nil {
					t.Fatalf("Send on %s: %v", side.from.Name(), err)
				}

				transmitted, _ := nextEvent(t, capture, frameKey(side.from, shape.frame, gocan.DirectionTransmit), cursor)
				if transmitted.Direction != gocan.DirectionTransmit || transmitted.Frame != shape.frame {
					t.Fatalf("TX event %+v does not match the sent frame", transmitted)
				}
				received, _ := nextEvent(t, capture, frameKey(side.to, shape.frame, gocan.DirectionReceive), cursor)
				if received.Direction != gocan.DirectionReceive || received.Frame != shape.frame {
					t.Fatalf("RX event %+v does not match the sent frame", received)
				}
				if received.Timestamp.IsZero() {
					t.Fatal("RX event carries no timestamp")
				}
			}

			// No echo: each endpoint has one TX and one RX series occurrence.
			for _, expect := range []struct {
				bus       gocan.Bus
				direction gocan.Direction
			}{
				{a, gocan.DirectionTransmit},
				{a, gocan.DirectionReceive},
				{b, gocan.DirectionTransmit},
				{b, gocan.DirectionReceive},
			} {
				series := capture.Series(frameKey(expect.bus, shape.frame, expect.direction))
				if len(series) != 1 || series[0].Direction != expect.direction {
					t.Fatalf("%v series on %s has %d events, want exactly one",
						expect.direction, expect.bus.Name(), len(series))
				}
			}
		})
	}
}

func testConcurrentTraffic(t *testing.T, open Pair) {
	capture, a, b := openPair(t, open)
	requirePeer(t, b)

	const senders, perSender = 4, 25
	var sending sync.WaitGroup
	for g := range senders {
		sending.Add(1)
		go func() {
			defer sending.Done()
			for seq := range perSender {
				data := make([]byte, 8)
				binary.LittleEndian.PutUint32(data, uint32(seq))
				frame, err := gocan.NewFrame(uint32(0x300+g), data, 0)
				if err != nil {
					t.Errorf("NewFrame: %v", err)
					return
				}
				if err := a.Send(context.Background(), frame); err != nil {
					t.Errorf("Send %d/%d: %v", g, seq, err)
					return
				}
			}
		}()
	}
	sending.Wait()

	// Every frame arrives at the peer exactly once, in per-series order.
	for g := range senders {
		key := gocan.FrameKey{Bus: b.ID(), ID: uint32(0x300 + g), Direction: gocan.DirectionReceive}
		var cursor gocan.Cursor
		for seq := range perSender {
			var event gocan.FrameEvent
			event, cursor = nextEvent(t, capture, key, cursor)
			if event.Direction != gocan.DirectionReceive {
				t.Fatalf("series %#x event %d has direction %v, want receive", key.ID, seq, event.Direction)
			}
			if got := binary.LittleEndian.Uint32(event.Frame.Data[:4]); got != uint32(seq) {
				t.Fatalf("series %#x out of order: event %d has sequence %d", key.ID, seq, got)
			}
		}
		if series := capture.Series(key); len(series) != perSender {
			t.Fatalf("series %#x holds %d events, want exactly %d", key.ID, len(series), perSender)
		}
		if series := capture.Series(gocan.FrameKey{Bus: a.ID(), ID: key.ID, Direction: gocan.DirectionTransmit}); len(series) != perSender {
			t.Fatalf("TX series %#x holds %d events, want exactly %d", key.ID, len(series), perSender)
		}
	}
}

func testOverrun(t *testing.T, open Pair, caps Capabilities) {
	if caps.InduceOverrun == nil {
		t.Skip("medium cannot induce a receive overrun")
	}
	capture, a, b := openPair(t, open)
	requirePeer(t, b)

	caps.InduceOverrun(t, a, b)
	requireClosed(t, b)
	if err := b.Err(); !errors.Is(err, gocan.ErrReceiveOverrun) {
		t.Fatalf("Err after overrun = %v, want ErrReceiveOverrun", err)
	}
	events := capture.BusEvents(b.ID())
	if len(events) == 0 || events[len(events)-1].Kind != gocan.EventReceiveOverrun {
		t.Fatalf("last bus event after overrun = %+v, want EventReceiveOverrun", events)
	}

	frame, err := gocan.NewFrame(0x100, []byte{1}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := b.Send(context.Background(), frame); !errors.Is(err, gocan.ErrReceiveOverrun) && !errors.Is(err, gocan.ErrBusClosed) {
		t.Fatalf("Send on overrun bus = %v, want the stop reason", err)
	}

	// The failure is contained: the peer and the capture keep working.
	if err := a.Send(context.Background(), frame); err != nil {
		t.Fatalf("peer Send after overrun: %v", err)
	}
	nextEvent(t, capture, frameKey(a, frame, gocan.DirectionTransmit), gocan.Cursor{})
}
