package virtual

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

func TestNetworkCapturesAcceptedTransmissionAndReception(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network

	source, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "source"})
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	destination, err := network.Open(context.Background(), capture, Config{ID: 2, Name: "destination"})
	if err != nil {
		t.Fatalf("Open destination: %v", err)
	}
	t.Cleanup(func() { _ = destination.Close() })

	frame, err := gocan.NewFrame(0x123, []byte{1, 2, 3, 4}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	beforeSend := capture.End()
	if err := source.Send(context.Background(), frame); err != nil {
		t.Fatalf("Send: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	transmitted, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       source.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionTransmit,
	}, beforeSend)
	if err != nil {
		t.Fatalf("Next transmitted: %v", err)
	}
	if transmitted.Direction != gocan.DirectionTransmit || transmitted.Frame != frame {
		t.Fatalf("transmitted event = %+v, want source TX frame", transmitted)
	}

	received, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       destination.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionReceive,
	}, beforeSend)
	if err != nil {
		t.Fatalf("Next received: %v", err)
	}
	if received.Direction != gocan.DirectionReceive || received.Frame != frame {
		t.Fatalf("received event = %+v, want destination RX frame", received)
	}
	if received.Timestamp.Before(transmitted.Timestamp) {
		t.Fatalf("RX timestamp %s precedes TX timestamp %s", received.Timestamp, transmitted.Timestamp)
	}

	seq, _, _ := capture.Series(gocan.FrameKey{
		Bus:       source.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionTransmit,
	}, gocan.Range{})
	if series := slices.Collect(seq); len(series) != 1 {
		t.Fatalf("source series has %d events, want TX only", len(series))
	}
}

func TestNetworkOpenHonorsCanceledContext(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	bus, err := network.Open(ctx, capture, Config{ID: 1, Name: "canceled"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v, want context.Canceled", err)
	}
	if bus != nil {
		t.Fatal("Open returned a bus for a canceled context")
	}
}

func TestBusLifecycleIsIndependentFromSharedCapture(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network

	first, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "first"})
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	second, err := network.Open(context.Background(), capture, Config{
		ID:                 2,
		Name:               "second",
		ReceiveOwnMessages: true,
	})
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if _, err := network.Open(context.Background(), capture, Config{ID: 2, Name: "duplicate"}); err == nil {
		t.Fatal("Open accepted a duplicate bus ID")
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	if cause := context.Cause(first.Context()); !errors.Is(cause, gocan.ErrBusClosed) {
		t.Fatalf("first Context cause after normal close = %v, want ErrBusClosed", cause)
	}
	if err := first.Send(context.Background(), gocan.Frame{}); !errors.Is(err, gocan.ErrBusClosed) {
		t.Fatalf("Send after close error = %v, want ErrBusClosed", err)
	}

	frame, err := gocan.NewFrame(0x321, []byte{9}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	cursor := capture.End()
	if err := second.Send(context.Background(), frame); err != nil {
		t.Fatalf("second Send after first closed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	transmitted, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       second.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionTransmit,
	}, cursor)
	if err != nil {
		t.Fatalf("wait for self transmission: %v", err)
	}
	received, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       second.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionReceive,
	}, cursor)
	if err != nil {
		t.Fatalf("wait for self reception: %v", err)
	}
	if transmitted.Direction != gocan.DirectionTransmit || received.Direction != gocan.DirectionReceive {
		t.Fatalf("self-reception directions = %v, %v, want TX then RX", transmitted.Direction, received.Direction)
	}
}

// TestBusOffDropsPeerTrafficAndOrdersState verifies that a bus-off endpoint
// drops peer frames instead of queuing them for recovery, and that accepted
// transmissions never appear inside a bus-off interval in Capture.
func TestBusOffDropsPeerTrafficAndOrdersState(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network
	sender, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "sender"})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := network.Open(context.Background(), capture, Config{ID: 2, Name: "receiver"})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	before, _ := gocan.NewFrame(0x520, []byte{1}, 0)
	dropped, _ := gocan.NewFrame(0x521, []byte{2}, 0)
	during, _ := gocan.NewFrame(0x522, []byte{3}, 0)
	after, _ := gocan.NewFrame(0x523, []byte{4}, 0)

	if err := sender.Send(context.Background(), before); err != nil {
		t.Fatalf("Send before bus-off: %v", err)
	}
	if err := sender.SetControllerState(gocan.ControllerBusOff); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Send(context.Background(), dropped); err != nil {
		t.Fatalf("peer Send during bus-off: %v", err)
	}
	if err := sender.Send(context.Background(), during); !errors.Is(err, gocan.ErrBusOff) {
		t.Fatalf("Send during bus-off = %v, want ErrBusOff", err)
	}
	if err := sender.SetControllerState(gocan.ControllerActive); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), after); err != nil {
		t.Fatalf("Send after recovery: %v", err)
	}

	rxSeq, _, _ := capture.Series(gocan.FrameKey{Bus: sender.ID(), ID: dropped.ID, Direction: gocan.DirectionReceive}, gocan.Range{})
	if got := len(slices.Collect(rxSeq)); got != 0 {
		t.Fatalf("sender received %d frames sent during bus-off, want 0", got)
	}
	checker := &busOffOrderChecker{t: t}
	if _, err := capture.WriteRecords(gocan.Range{}, checker); err != nil {
		t.Fatal(err)
	}
	if checker.txSeen != 2 {
		t.Fatalf("saw %d transmissions in capture, want 2", checker.txSeen)
	}
}

// TestClosePreservesPendingOverrun verifies that closing an endpoint right
// after its receive queue overflows does not drop the overrun event.
func TestClosePreservesPendingOverrun(t *testing.T) {
	for range 50 {
		capture := gocan.NewCapture()
		var network Network
		sender, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "sender"})
		if err != nil {
			t.Fatal(err)
		}
		target, err := network.Open(context.Background(), capture, Config{ID: 2, Name: "target"})
		if err != nil {
			_ = sender.Close()
			t.Fatal(err)
		}
		frame, _ := gocan.NewFrame(0x530, []byte{1}, 0)
		// Hold stateMu so target.run blocks on the first incoming frame while
		// the remaining sends fill incoming and overflow into target.overrun.
		target.stateMu.Lock()
		for range receiveQueueCapacity + 2 {
			if err := sender.Send(context.Background(), frame); err != nil {
				target.stateMu.Unlock()
				t.Fatal(err)
			}
		}
		target.stateMu.Unlock()
		_ = target.Close()
		_ = sender.Close()

		events, _, _ := capture.BusEvents(target.ID(), gocan.Range{})
		if !slices.ContainsFunc(slices.Collect(events), func(e gocan.Event) bool {
			return e.Kind == gocan.EventReceiveOverrun
		}) {
			t.Fatal("Close dropped confirmed EventReceiveOverrun")
		}
	}
}

type busOffOrderChecker struct {
	t      *testing.T
	busOff bool
	txSeen int
}

func (c *busOffOrderChecker) WriteFrame(frame gocan.FrameEvent) error {
	if frame.Bus == 1 && frame.Direction == gocan.DirectionTransmit {
		if c.busOff {
			c.t.Fatalf("transmission recorded while bus-off: %+v", frame)
		}
		c.txSeen++
	}
	return nil
}

func (c *busOffOrderChecker) WriteEvent(event gocan.Event) error {
	if event.Bus == 1 && event.Kind == gocan.EventControllerState {
		c.busOff = event.ControllerState == gocan.ControllerBusOff
	}
	return nil
}
