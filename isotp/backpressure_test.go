package isotp_test

import (
	"bytes"
	"context"
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
		if _, err := isotp.New(senderBus, isotp.Config{TransmitID: 0x700, ReceiveID: 0x708, TransmitRetryTimeout: -1}); err == nil {
			t.Fatal("New accepted a negative retry timeout")
		}
		if _, err := isotp.NewFunctional(senderBus, isotp.FunctionalConfig{TransmitID: 0x7df, TransmitRetryTimeout: -1}); err == nil {
			t.Fatal("NewFunctional accepted a negative retry timeout")
		}
		sender, err := isotp.New(senderBus, isotp.Config{TransmitID: 0x700, ReceiveID: 0x708, TransmitRetryTimeout: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		receiver, err := isotp.New(receiverBus, isotp.Config{TransmitID: 0x708, ReceiveID: 0x700, TransmitRetryTimeout: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		inject := func(data ...byte) {
			frame, _ := gocan.NewFrame(0x708, data, 0)
			if err := senderBus.Capture().RecordFrame(gocan.FrameEvent{Bus: 1, Direction: gocan.DirectionReceive, Frame: frame}); err != nil {
				t.Fatal(err)
			}
		}

		// Every frame in both directions is rejected once before acceptance.
		rejectOnce := func(bus *backpressureBus) {
			rejected := false
			bus.reject = func(gocan.Frame) error {
				rejected = !rejected
				if !rejected {
					return nil
				}
				return fmt.Errorf("adapter: %w", gocan.ErrTransmitQueueFull)
			}
		}
		rejectOnce(senderBus)
		rejectOnce(receiverBus)
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
		// 4002 bytes at DLC 8: a First Frame of 6 bytes, then 571 Consecutive
		// Frames of 7. The receiver answers with one Flow Control.
		if senderBus.accepted != 572 || receiverBus.accepted != 1 {
			t.Fatalf("accepted frames: sender=%d receiver=%d", senderBus.accepted, receiverBus.accepted)
		}

		// A reply captured during rejection predates the request and is skipped;
		// one captured before the accepted Send returns is kept.
		senderBus.reject = func(gocan.Frame) error {
			inject(1, 0x99)
			senderBus.reject = nil
			return gocan.ErrTransmitQueueFull
		}
		senderBus.after = func() { inject(1, 0x42) }
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
	after    func()
	accepted int
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
	if bus.after != nil {
		bus.after()
	}
	return nil
}
