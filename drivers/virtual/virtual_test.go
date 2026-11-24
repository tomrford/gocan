package virtual

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
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

// TestBusOffRecoveryWait measures gocan.Send across a bus-off as long as the
// PCAN-USB one seen on the bench: CAN_Write rejected frames for about 1 s
// after a spurious bus-off (November 2026). The logged figures are exact
// under synctest's fake clock.
func TestBusOffRecoveryWait(t *testing.T) {
	// Recover between 10 ms retry ticks so synctest does not tie the recovery
	// callback with the 1.5 s retry attempt.
	const (
		busOff     = 1495 * time.Millisecond
		wantAccept = 1500 * time.Millisecond
	)
	for _, test := range []struct {
		budget time.Duration
		accept bool
	}{{2 * time.Second, true}, {time.Second, false}} {
		synctest.Test(t, func(t *testing.T) {
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

			if err := sender.SetControllerState(gocan.ControllerBusOff); err != nil {
				t.Fatal(err)
			}
			// Traffic sent by a peer while sender is bus-off must be dropped,
			// not delivered after recovery.
			dropped, err := gocan.NewFrame(0x51f, []byte{9}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := receiver.Send(context.Background(), dropped); err != nil {
				t.Fatalf("peer Send during bus-off: %v", err)
			}
			time.AfterFunc(busOff, func() { _ = sender.SetControllerState(gocan.ControllerActive) })
			counted := &countingBus{Bus: sender}
			frame, err := gocan.NewFrame(0x51e, []byte{1}, 0)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			err = gocan.Send(context.Background(), counted, frame, test.budget)
			elapsed := time.Since(start)
			t.Logf("bus-off %v, budget %v: %v after %v and %d attempts", busOff, test.budget, err, elapsed, counted.attempts)

			txSeq, _, _ := capture.Series(gocan.FrameKey{Bus: sender.ID(), ID: frame.ID, Direction: gocan.DirectionTransmit}, gocan.Range{})
			sent := len(slices.Collect(txSeq))
			if test.accept {
				if err != nil || elapsed != wantAccept || sent != 1 {
					t.Fatalf("Send = %v after %v with %d transmissions; want acceptance at %v", err, elapsed, sent, wantAccept)
				}
				rxSeq, _, _ := capture.Series(gocan.FrameKey{Bus: sender.ID(), ID: dropped.ID, Direction: gocan.DirectionReceive}, gocan.Range{})
				if got := len(slices.Collect(rxSeq)); got != 0 {
					t.Fatalf("sender received %d frames sent during bus-off, want 0", got)
				}
			} else if !errors.Is(err, gocan.ErrBusOff) || elapsed != test.budget || sent != 0 {
				t.Fatalf("Send = %v after %v with %d transmissions; want ErrBusOff at %v", err, elapsed, sent, test.budget)
			}
			// Polling every 10 ms keeps a 1.5 s wait to about 150 native calls.
			if limit := int(min(elapsed, test.budget)/(10*time.Millisecond)) + 1; counted.attempts > limit {
				t.Fatalf("%d attempts in %v, want at most %d", counted.attempts, elapsed, limit)
			}
		})
	}
}

func TestBusOffSerializesTransmissionsWithStateEvents(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network
	bus, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "serial"})
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()

	frame, err := gocan.NewFrame(0x520, []byte{1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := bus.Send(context.Background(), frame); err != nil && !errors.Is(err, gocan.ErrBusOff) {
				done <- err
				return
			}
		}
	}()
	for range 200 {
		if err := bus.SetControllerState(gocan.ControllerBusOff); err != nil {
			t.Fatal(err)
		}
		if err := bus.SetControllerState(gocan.ControllerActive); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("concurrent Send: %v", err)
	}

	checker := &busOffOrderChecker{t: t}
	if _, err := capture.WriteRecords(gocan.Range{}, checker); err != nil {
		t.Fatal(err)
	}
}

type busOffOrderChecker struct {
	t      *testing.T
	busOff bool
}

func (c *busOffOrderChecker) WriteFrame(frame gocan.FrameEvent) error {
	if c.busOff && frame.Direction == gocan.DirectionTransmit {
		c.t.Fatalf("transmission recorded while bus-off: %+v", frame)
	}
	return nil
}

func (c *busOffOrderChecker) WriteEvent(event gocan.Event) error {
	if event.Kind == gocan.EventControllerState {
		c.busOff = event.ControllerState == gocan.ControllerBusOff
	}
	return nil
}

type countingBus struct {
	*Bus
	attempts int
}

func (bus *countingBus) Send(ctx context.Context, frame gocan.Frame) error {
	bus.attempts++
	return bus.Bus.Send(ctx, frame)
}
