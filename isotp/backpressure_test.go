package isotp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/internal/driverstate"
	"github.com/tomrford/gocan/isotp"
)

func TestTransmitBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var network virtual.Network
		open := func(id gocan.BusID) *backpressureBus {
			bus, err := network.Open(context.Background(), gocan.NewCapture(), virtual.Config{ID: id, Name: fmt.Sprint(id)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = bus.Close() })
			return &backpressureBus{Bus: bus}
		}
		senderBus, receiverBus := open(1), open(2)
		sender, err := isotp.New(senderBus, isotp.Config{TransmitID: 0x700, ReceiveID: 0x708, TransmitRetryTimeout: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		receiver, err := isotp.New(receiverBus, isotp.Config{TransmitID: 0x708, ReceiveID: 0x700})
		if err != nil {
			t.Fatal(err)
		}

		// Reject the first frame and stall partway through a long transfer.
		// The receiver also encounters backpressure sending Flow Control.
		senderBus.reject = func(frame gocan.Frame) error {
			if senderBus.accepted == 0 && senderBus.retries == 0 {
				stale, _ := gocan.NewFrame(0x708, []byte{0x32, 0, 0}, 0)
				if err := senderBus.Capture().RecordFrame(gocan.FrameEvent{Bus: 1, Direction: gocan.DirectionReceive, Frame: stale}); err != nil {
					t.Fatal(err)
				}
			}
			if senderBus.accepted == 0 || senderBus.accepted == 357 {
				if senderBus.retries < 3 {
					senderBus.retries++
					return fmt.Errorf("adapter: %w", gocan.ErrTransmitQueueFull)
				}
			}
			senderBus.retries = 0
			return nil
		}
		receiverBus.reject = func(gocan.Frame) error {
			if receiverBus.retries == 0 {
				receiverBus.retries++
				return gocan.ErrTransmitQueueFull
			}
			return nil
		}
		payload := patternedPayload(4002, 0x36)
		sent := make(chan error, 1)
		go func() { sent <- sender.Send(context.Background(), payload) }()
		got, err := receiver.Receive(context.Background())
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("Receive: %v, payload matches: %v", err, bytes.Equal(got, payload))
		}
		if err := <-sent; err != nil {
			t.Fatal(err)
		}
		if senderBus.accepted != 572 || receiverBus.accepted != 1 {
			t.Fatalf("accepted frames: sender=%d receiver=%d", senderBus.accepted, receiverBus.accepted)
		}

		// A stalled adapter is bounded even without a caller deadline; an
		// earlier cancellation must release the link for the next operation.
		for _, cancelEarly := range []bool{false, true} {
			senderBus.reject = func(gocan.Frame) error { return gocan.ErrTransmitQueueFull }
			ctx, cancel := context.WithCancel(context.Background())
			if cancelEarly {
				time.AfterFunc(5*time.Millisecond, cancel)
			}
			start := time.Now()
			err := sender.Send(ctx, []byte{0x3e, 0})
			cancel()
			want := context.DeadlineExceeded
			wantElapsed := 20 * time.Millisecond
			if cancelEarly {
				want = context.Canceled
				wantElapsed = 5 * time.Millisecond
			}
			if !errors.Is(err, want) || time.Since(start) != wantElapsed {
				t.Fatalf("stalled Send: %v after %v, want %v after %v", err, time.Since(start), want, wantElapsed)
			}
		}
		senderBus.reject = func(gocan.Frame) error { return gocan.ErrBusOff }
		start := time.Now()
		if err := sender.Send(context.Background(), []byte{0x3e, 0}); !errors.Is(err, gocan.ErrBusOff) || time.Now() != start {
			t.Fatalf("fatal Send: %v", err)
		}
		senderBus.reject = nil
		if err := sender.Send(context.Background(), []byte{0x3e, 0}); err != nil {
			t.Fatalf("Send after recovery: %v", err)
		}
		functional, err := isotp.NewFunctional(senderBus, isotp.FunctionalConfig{TransmitID: 0x7df, TransmitRetryTimeout: 10 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		senderBus.reject = func(gocan.Frame) error {
			if time.Since(start) < 3*time.Millisecond {
				return gocan.ErrTransmitQueueFull
			}
			return nil
		}
		before := senderBus.accepted
		if err := functional.Send(context.Background(), []byte{0x3e, 0}); err != nil || senderBus.accepted != before+1 {
			t.Fatalf("functional Send: %v, accepted %d frames", err, senderBus.accepted-before)
		}
		// A stale payload during rejection must be skipped, while a valid reply
		// captured before the accepted Send returns must remain visible.
		inject := func(value byte) {
			frame, _ := gocan.NewFrame(0x708, []byte{1, value}, 0)
			if err := senderBus.Capture().RecordFrame(gocan.FrameEvent{Bus: 1, Direction: gocan.DirectionReceive, Frame: frame}); err != nil {
				t.Fatal(err)
			}
		}
		senderBus.reject = func(gocan.Frame) error {
			inject(0x99)
			senderBus.reject = nil
			return gocan.ErrTransmitQueueFull
		}
		senderBus.after = func() { inject(0x42) }
		exchange, err := sender.Begin(context.Background(), []byte{0x22})
		if err != nil {
			t.Fatal(err)
		}
		got, err = exchange.Next(context.Background(), time.Second)
		exchange.Close()
		if err != nil || !bytes.Equal(got, []byte{0x42}) {
			t.Fatalf("reply after retry: %x, %v", got, err)
		}
	})
}

type backpressureBus struct {
	gocan.Bus
	reject   func(gocan.Frame) error
	accepted int
	retries  int
	after    func()
}

func (bus *backpressureBus) Send(ctx context.Context, frame gocan.Frame, retryTimeout time.Duration) error {
	return driverstate.Send(ctx, bus, frame, retryTimeout, bus.send)
}

func (bus *backpressureBus) send(ctx context.Context, frame gocan.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if bus.reject != nil {
		if err := bus.reject(frame); err != nil {
			return err
		}
	}
	if err := bus.Bus.Send(ctx, frame, 0); err != nil {
		return err
	}
	bus.accepted++
	if bus.after != nil {
		bus.after()
	}
	return nil
}
