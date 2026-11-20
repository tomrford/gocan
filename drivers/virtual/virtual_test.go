package virtual

import (
	"context"
	"errors"
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

	if series := capture.Series(gocan.FrameKey{
		Bus:       source.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionTransmit,
	}); len(series) != 1 {
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
	if first.Err() != nil {
		t.Fatalf("first Err after normal close = %v, want nil", first.Err())
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
	const busOff = 1500 * time.Millisecond
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

			sent := len(capture.Series(gocan.FrameKey{Bus: sender.ID(), ID: frame.ID, Direction: gocan.DirectionTransmit}))
			if test.accept {
				if err != nil || elapsed != busOff || sent != 1 {
					t.Fatalf("Send = %v after %v with %d transmissions; want acceptance at %v", err, elapsed, sent, busOff)
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

type countingBus struct {
	*Bus
	attempts int
}

func (bus *countingBus) Send(ctx context.Context, frame gocan.Frame) error {
	bus.attempts++
	return bus.Bus.Send(ctx, frame)
}
