//go:build windows && amd64

package vector

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

func TestVectorCrossAdapterPhysicalDisconnect(t *testing.T) {
	if os.Getenv("GOCAN_VECTOR_DISCONNECT_TEST") != "1" {
		t.Skip("GOCAN_VECTOR_DISCONNECT_TEST=1 is not set")
	}
	vectorA, vectorB := vectorPairIndexes(t)
	channels, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	serials := make(map[ChannelIndex]uint32, len(channels))
	for _, channel := range channels {
		serials[channel.ChannelIndex] = channel.SerialNumber
	}
	serialA, foundA := serials[vectorA]
	serialB, foundB := serials[vectorB]
	if !foundA || !foundB {
		t.Fatalf("Vector channels %d and %d not both present in %+v", vectorA, vectorB, channels)
	}
	if serialA == serialB {
		t.Skip("select channels on different physical Vector adapters")
	}

	capture := gocan.NewCapture()
	first, err := Open(context.Background(), capture, Config{
		ID: 1, Name: "vector-disconnect-a", ChannelIndex: vectorA,
		Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("open first Vector channel: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(context.Background(), capture, Config{
		ID: 2, Name: "vector-disconnect-b", ChannelIndex: vectorB,
		Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("open second Vector channel: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	cursor := capture.End()
	frame, err := gocan.NewFrame(0x5a8, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := first.Send(context.Background(), frame); err != nil {
		t.Fatalf("send before disconnect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       second.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionReceive,
	}, cursor); err != nil {
		cancel()
		t.Fatalf("receive before disconnect: %v", err)
	}
	cancel()

	t.Log("initial traffic passed; disconnect the selected Vector USB adapter now")
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	var sendErr error
	for sendErr == nil {
		select {
		case <-first.Done():
			sendErr = first.Err()
		case <-second.Done():
			sendErr = second.Err()
		case <-ticker.C:
			sendErr = first.Send(context.Background(), frame)
		case <-deadline.C:
			t.Fatal("timed out waiting for the selected Vector USB adapter to disconnect")
		}
	}
	t.Logf("first disconnect observation: %v", sendErr)

	select {
	case <-first.Done():
		if !errors.Is(first.Err(), gocan.ErrHardwareDisconnected) {
			t.Fatalf("Vector bus %q Err() = %v, want ErrHardwareDisconnected", first.Name(), first.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Vector bus %q remained open after disconnect; first observation: %v", first.Name(), sendErr)
	}
	select {
	case <-second.Done():
		t.Fatalf("Vector bus %q on a different adapter stopped after disconnect: %v", second.Name(), second.Err())
	case <-time.After(2 * chipStateHealthInterval):
	}
}
