package uds_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/isotp"
	"github.com/tomrford/gocan/uds"
)

func TestRetentionAcrossPendingAndCancelledRequests(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	bus, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	link, err := isotp.New(bus, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
	if err != nil {
		t.Fatal(err)
	}
	client, err := uds.New(link, uds.Config{P2Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := capture.End()
	result := make(chan error, 1)
	go func() { _, err := client.Do(ctx, uds.Request{Service: 0x22}); result <- err }()
	if _, _, err := capture.Next(ctx, gocan.FrameKey{Bus: 1, ID: 0x7e0, Direction: gocan.DirectionTransmit}, start); err != nil {
		t.Fatal(err)
	}
	if got := client.RetentionCursor(); got != start {
		t.Fatal("pending request released its receive history")
	}
	queued, stopQueued := context.WithCancel(ctx)
	stopQueued()
	if _, err := client.Do(queued, uds.Request{Service: 0x22}); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued request: %v", err)
	}
	if client.RetentionCursor() != start {
		t.Fatal("cancelled queued request released the active request's history")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("active request: %v", err)
	}
	if got := client.RetentionCursor(); got != capture.End() || got == start {
		t.Fatal("idle client did not release old history")
	}
}

func TestSendRetentionAfterIdlePruning(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	bus, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	capture.Clear()
	observed := &retentionBus{Bus: bus}
	link, err := isotp.New(observed, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
	if err != nil {
		t.Fatal(err)
	}
	client, err := uds.New(link, uds.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	event := gocan.Event{Bus: 1, Timestamp: time.Now(), Kind: gocan.EventErrorFrame}
	for _, stage := range []string{"initially empty", "after idle pruning"} {
		t.Run(stage, func(t *testing.T) {
			// Enough traffic to seal chunks, so Prune must actually free history.
			for range 1 << 18 {
				if err := capture.AppendEvent(event); err != nil {
					t.Fatal(err)
				}
			}
			start := capture.End()
			observed.beforeSend = func() {
				if got := client.RetentionCursor(); got != start {
					t.Errorf("Send retained %v, want current boundary %v", got, start)
				}
				before := capture.Len()
				if err := capture.Prune(client.RetentionCursor()); err != nil {
					t.Errorf("Prune during Send: %v", err)
				} else if capture.Len() >= before {
					t.Error("Send prevented pruning old history")
				}
			}
			if err := client.SendTesterPresent(ctx); err != nil {
				t.Fatal(err)
			}
			if client.RetentionCursor() != capture.End() {
				t.Fatal("Send did not release history on completion")
			}
			// Discard the link's previous position while the client is idle.
			for range 1 << 18 {
				if err := capture.AppendEvent(event); err != nil {
					t.Fatal(err)
				}
			}
			if err := capture.Prune(client.RetentionCursor()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type retentionBus struct {
	gocan.Bus
	beforeSend func()
}

func (bus *retentionBus) Send(ctx context.Context, frame gocan.Frame) error {
	bus.beforeSend()
	return bus.Bus.Send(ctx, frame)
}
