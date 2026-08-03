//go:build linux

package socketcan

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"golang.org/x/sys/unix"
)

// BenchmarkVCanCaptureFD64 measures the complete receive path from a raw vcan
// write through SocketCAN decoding and storage in one shared Capture.
func BenchmarkVCanCaptureFD64(b *testing.B) {
	iface := os.Getenv("GOCAN_VCAN_INTERFACE")
	if iface == "" {
		b.Skip("GOCAN_VCAN_INTERFACE is not set")
	}

	for _, busCount := range []int{1, 3} {
		b.Run(fmt.Sprintf("buses=%d", busCount), func(b *testing.B) {
			capture := gocan.NewCapture()
			var buses []*Bus
			defer func() {
				for _, bus := range buses {
					_ = bus.Close()
				}
			}()
			for index := range busCount {
				bus, err := Open(context.Background(), capture, Config{
					ID:        gocan.BusID(index + 1),
					Name:      fmt.Sprintf("saturation-%d", index+1),
					Interface: iface,
					FD:        true,
				})
				if err != nil {
					b.Fatalf("Open bus %d: %v", index+1, err)
				}
				buses = append(buses, bus)
			}

			sender := openErrorSender(b, iface)
			if err := unix.SetsockoptInt(sender, unix.SOL_CAN_RAW, unix.CAN_RAW_FD_FRAMES, 1); err != nil {
				b.Fatalf("enable CAN FD on sender: %v", err)
			}
			frame, err := gocan.NewFrame(0x123, make([]byte, 64), gocan.FrameFD)
			if err != nil {
				b.Fatalf("NewFrame: %v", err)
			}
			var buffer [fdMTU]byte
			packet := encodeFrame(frame, &buffer)
			wantRecords := busCount * b.N

			b.SetBytes(64 * int64(busCount))
			b.ReportAllocs()
			b.ResetTimer()
			started := time.Now()
			for index := range b.N {
				binary.LittleEndian.PutUint64(packet[8:], uint64(index))
				written, err := unix.Write(sender, packet)
				if err != nil {
					b.Fatalf("send frame %d: %v", index, err)
				}
				if written != len(packet) {
					b.Fatalf("send frame %d wrote %d of %d bytes", index, written, len(packet))
				}
				if (index+1)%128 == 0 {
					waitForCaptureRecords(b, capture, busCount*(index+1))
				}
			}
			waitForCaptureRecords(b, capture, wantRecords)
			elapsed := time.Since(started)
			b.StopTimer()

			if got := capture.Len(); got != wantRecords {
				b.Fatalf("captured %d of %d records", got, wantRecords)
			}
			b.ReportMetric(float64(wantRecords)/elapsed.Seconds(), "records/s")
		})
	}
}

func waitForCaptureRecords(b *testing.B, capture *gocan.Capture, want int) {
	b.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for capture.Len() < want && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := capture.Len(); got != want {
		b.Fatalf("captured %d of %d records", got, want)
	}
}
