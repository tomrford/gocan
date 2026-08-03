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

func TestVectorPhysicalDisconnect(t *testing.T) {
	if os.Getenv("GOCAN_VECTOR_DISCONNECT_TEST") != "1" {
		t.Skip("GOCAN_VECTOR_DISCONNECT_TEST=1 is not set")
	}
	vectorA, vectorB := vectorPairIndexes(t)
	dataBitrate := vectorFDDataBitrate(t)

	capture := gocan.NewCapture()
	first, err := Open(context.Background(), capture, Config{
		ID: 1, Name: "vector-disconnect-a", ChannelIndex: vectorA,
		Bitrate: 500_000, DataBitrate: dataBitrate,
	})
	if err != nil {
		t.Fatalf("open first Vector channel: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(context.Background(), capture, Config{
		ID: 2, Name: "vector-disconnect-b", ChannelIndex: vectorB,
		Bitrate: 500_000, DataBitrate: dataBitrate,
	})
	if err != nil {
		t.Fatalf("open second Vector channel: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	cursor := capture.End()
	frame, err := vectorEventFrame(
		0x5a8,
		12,
		1,
		gocan.FrameFD|gocan.FrameBitRateSwitch,
	)
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

	t.Log("initial FD traffic passed; disconnect the selected Vector USB adapter now")
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

	for _, bus := range []gocan.Bus{first, second} {
		select {
		case <-bus.Done():
			if !errors.Is(bus.Err(), gocan.ErrHardwareDisconnected) {
				t.Fatalf("Vector bus %q Err() = %v, want ErrHardwareDisconnected", bus.Name(), bus.Err())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Vector bus %q remained open after disconnect; first observation: %v", bus.Name(), sendErr)
		}
	}
}
