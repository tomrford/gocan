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
