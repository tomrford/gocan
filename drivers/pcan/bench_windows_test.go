//go:build windows

package pcan

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
)

func benchPCANPair(b *testing.B) (Channel, Channel) {
	b.Helper()
	channelAValue := os.Getenv("GOCAN_PCAN_CHANNEL_A")
	channelBValue := os.Getenv("GOCAN_PCAN_CHANNEL_B")
	if channelAValue == "" || channelBValue == "" {
		b.Skip("GOCAN_PCAN_CHANNEL_A and GOCAN_PCAN_CHANNEL_B are not set")
	}
	parse := func(name, value string) Channel {
		channel, err := strconv.ParseUint(value, 0, 16)
		if err != nil || channel == 0 {
			b.Fatalf("%s=%q is not a nonzero 16-bit numeric PCAN handle", name, value)
		}
		return Channel(channel)
	}
	return parse("GOCAN_PCAN_CHANNEL_A", channelAValue), parse("GOCAN_PCAN_CHANNEL_B", channelBValue)
}

func benchPCANBuses(b *testing.B, frameID uint32) (*gocan.Capture, gocan.Bus, gocan.Bus, gocan.Frame) {
	b.Helper()
	channelA, channelB := benchPCANPair(b)
	ctx := context.Background()
	capture := gocan.NewCapture()

	sender, err := Open(ctx, capture, Config{ID: 1, Name: "bench-tx", Channel: channelA, Bitrate: Bitrate500K})
	if err != nil {
		b.Fatalf("Open sender: %v", err)
	}
	b.Cleanup(func() { _ = sender.Close() })
	receiver, err := Open(ctx, capture, Config{ID: 2, Name: "bench-rx", Channel: channelB, Bitrate: Bitrate500K})
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
