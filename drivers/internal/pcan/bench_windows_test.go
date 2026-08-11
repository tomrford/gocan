//go:build windows

package pcan

import (
	"context"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
)

func benchPCANBuses(b *testing.B, frameID uint32) (*gocan.Capture, gocan.Bus, gocan.Bus, gocan.Frame) {
	b.Helper()
	channelA := testChannel(b, "GOCAN_PCAN_CHANNEL_A")
	channelB := testChannel(b, "GOCAN_PCAN_CHANNEL_B")
	ctx := context.Background()
	capture := gocan.NewCapture()

	sender, err := Open(ctx, capture, Config{ID: 1, Name: "bench-tx", Channel: channelA, Bitrate: 500_000})
	if err != nil {
		b.Fatalf("Open sender: %v", err)
	}
	b.Cleanup(func() { _ = sender.Close() })
	receiver, err := Open(ctx, capture, Config{ID: 2, Name: "bench-rx", Channel: channelB, Bitrate: 500_000})
	if err != nil {
		b.Fatalf("Open receiver: %v", err)
	}
	b.Cleanup(func() { _ = receiver.Close() })

	frame, err := gocan.NewFrame(frameID, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if err != nil {
		b.Fatalf("NewFrame: %v", err)
	}
	return capture, sender, receiver, frame
}

func BenchmarkPCANRoundTrip(b *testing.B) {
	capture, sender, receiver, frame := benchPCANBuses(b, 0x510)
	conformance.RoundTripBenchmark(b, capture, sender, receiver, frame)
}

func BenchmarkPCANSaturatedCapture(b *testing.B) {
	capture, sender, receiver, frame := benchPCANBuses(b, 0x511)
	conformance.SaturatedCaptureBenchmark(b, capture, sender, receiver, frame)
}
