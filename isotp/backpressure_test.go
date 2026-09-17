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
		sender, err := isotp.New(senderBus, isotp.Config{TransmitID: 0x700, ReceiveID: 0x708})
		if err != nil {
			t.Fatal(err)
		}
		receiver, err := isotp.New(receiverBus, isotp.Config{TransmitID: 0x708, ReceiveID: 0x700})
		if err != nil {
			t.Fatal(err)
		}

		// Reject the first frame, then stall after 356 accepted consecutive
		// frames, matching the Vector flash failure. Also exercise FC retries.
		senderBus.reject = func(frame gocan.Frame) error {
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
			err := sender.Send(ctx, []byte{0x3e, 0})
			cancel()
			want := context.DeadlineExceeded
			if cancelEarly {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("stalled Send: %v, want %v", err, want)
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
	})
}

type backpressureBus struct {
	gocan.Bus
	reject   func(gocan.Frame) error
	accepted int
	retries  int
}

func (bus *backpressureBus) Send(ctx context.Context, frame gocan.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if bus.reject != nil {
		if err := bus.reject(frame); err != nil {
			return err
		}
	}
	if err := bus.Bus.Send(ctx, frame); err != nil {
		return err
	}
	bus.accepted++
	return nil
}
