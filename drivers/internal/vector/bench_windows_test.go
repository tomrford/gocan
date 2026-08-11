//go:build windows && amd64

package vector

import (
	"context"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
	"github.com/tomrford/gocan/drivers/internal/pcan"
)

// benchVectorToPCAN opens one Vector classic channel as sender and one PCAN
// channel as receiver on the shared 500 kbit/s network.
func benchVectorToPCAN(b *testing.B, frameID uint32) (*gocan.Capture, gocan.Bus, gocan.Bus, gocan.Frame) {
	b.Helper()
	vectorIndex := vectorChannelIndex(b, "GOCAN_VECTOR_CHANNEL_INDEX")
	pcanChannel := pcanPeerChannel(b, "GOCAN_PCAN_CHANNEL")

	ctx := context.Background()
	capture := gocan.NewCapture()
	sender, err := Open(ctx, capture, Config{
		ID: 1, Name: "bench-vector-tx", ChannelIndex: vectorIndex, Bitrate: 500_000,
	})
	if err != nil {
		b.Fatalf("Open Vector sender: %v", err)
	}
	b.Cleanup(func() { _ = sender.Close() })
	receiver, err := pcan.Open(ctx, capture, pcan.Config{
		ID: 2, Name: "bench-pcan-rx", Channel: pcanChannel, Bitrate: 500_000,
	})
	if err != nil {
		b.Fatalf("Open PCAN receiver: %v", err)
	}
	b.Cleanup(func() { _ = receiver.Close() })

	frame, err := gocan.NewFrame(frameID, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if err != nil {
		b.Fatalf("NewFrame: %v", err)
	}
	return capture, sender, receiver, frame
}

func BenchmarkVectorRoundTrip(b *testing.B) {
	capture, sender, receiver, frame := benchVectorToPCAN(b, 0x520)
	conformance.RoundTripBenchmark(b, capture, sender, receiver, frame)
}

func BenchmarkVectorSaturatedCapture(b *testing.B) {
	capture, sender, receiver, frame := benchVectorToPCAN(b, 0x521)
	conformance.SaturatedCaptureBenchmark(b, capture, sender, receiver, frame)
}

// BenchmarkVectorFDSaturatedCapture measures sustained 64-byte BRS capture on
// two back-to-back Vector FD channels at 500 kbit/s nominal and the configured
// data bitrate.
func BenchmarkVectorFDSaturatedCapture(b *testing.B) {
	vectorA, vectorB := vectorPairIndexes(b)
	dataBitrate := vectorFDDataBitrate(b)

	ctx := context.Background()
	capture := gocan.NewCapture()
	sender, err := Open(ctx, capture, Config{
		ID: 1, Name: "bench-fd-tx", ChannelIndex: vectorA,
		FDTiming: vectorFDTiming(dataBitrate),
	})
	if err != nil {
		b.Fatalf("Open FD sender: %v", err)
	}
	b.Cleanup(func() { _ = sender.Close() })
	receiver, err := Open(ctx, capture, Config{
		ID: 2, Name: "bench-fd-rx", ChannelIndex: vectorB,
		FDTiming: vectorFDTiming(dataBitrate),
	})
	if err != nil {
		b.Fatalf("Open FD receiver: %v", err)
	}
	b.Cleanup(func() { _ = receiver.Close() })

	payload := make([]byte, 64)
	for index := range payload {
		payload[index] = byte(index)
	}
	frame, err := gocan.NewFrame(0x522, payload, gocan.FrameFD|gocan.FrameBitRateSwitch)
	if err != nil {
		b.Fatalf("NewFrame: %v", err)
	}
	conformance.SaturatedCaptureBenchmark(b, capture, sender, receiver, frame)
}
