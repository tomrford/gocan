//go:build windows

package zlg

import (
	"context"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
	"github.com/tomrford/gocan/drivers/internal/pcan"
)

func benchBuses(b *testing.B, frameID uint32) (*gocan.Capture, gocan.Bus, gocan.Bus, gocan.Frame) {
	b.Helper()
	zlgConfig, pcanConfig := hardwarePair(b)
	capture := gocan.NewCapture()
	zlgBus, err := Open(context.Background(), capture, zlgConfig)
	if err != nil {
		b.Fatalf("open ZLG: %v", err)
	}
	b.Cleanup(func() { _ = zlgBus.Close() })
	pcanBus, err := pcan.Open(context.Background(), capture, pcanConfig)
	if err != nil {
		b.Fatalf("open PCAN: %v", err)
	}
	b.Cleanup(func() { _ = pcanBus.Close() })
	frame, err := gocan.NewFrame(frameID, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if err != nil {
		b.Fatalf("NewFrame: %v", err)
	}
	return capture, zlgBus, pcanBus, frame
}

func BenchmarkZLGRoundTrip(b *testing.B) {
	capture, zlgBus, pcanBus, frame := benchBuses(b, 0x520)
	conformance.RoundTripBenchmark(b, capture, zlgBus, pcanBus, frame)
}

func BenchmarkZLGSaturatedTransmit(b *testing.B) {
	capture, zlgBus, pcanBus, frame := benchBuses(b, 0x521)
	conformance.SaturatedCaptureBenchmark(b, capture, zlgBus, pcanBus, frame)
}

func BenchmarkZLGSaturatedReceive(b *testing.B) {
	capture, zlgBus, pcanBus, frame := benchBuses(b, 0x522)
	conformance.SaturatedCaptureBenchmark(b, capture, pcanBus, zlgBus, frame)
}
