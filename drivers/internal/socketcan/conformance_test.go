//go:build linux

package socketcan

import (
	"context"
	"os"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
	"golang.org/x/sys/unix"
)

func TestVCanConformance(t *testing.T) {
	iface := os.Getenv("GOCAN_VCAN_INTERFACE")
	if iface == "" {
		t.Skip("GOCAN_VCAN_INTERFACE is not set")
	}

	open := func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		a, err := Open(context.Background(), capture, Config{
			ID:        1,
			Name:      "socketcan-a",
			Interface: iface,
			FD:        true,
		})
		if err != nil {
			t.Fatalf("Open a: %v", err)
		}
		t.Cleanup(func() { _ = a.Close() })

		b, err := Open(context.Background(), capture, Config{
			ID:        2,
			Name:      "socketcan-b",
			Interface: iface,
			FD:        true,
		})
		if err != nil {
			t.Fatalf("Open b: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return a, b
	}

	conformance.Run(t, open, conformance.Capabilities{
		FD:           true,
		RemoteFrames: true,
		InduceOverrun: func(t *testing.T, healthy, target gocan.Bus) {
			induceVCanOverrun(t, healthy.(*Bus), target.(*Bus), iface)
		},
	})
}

func induceVCanOverrun(t *testing.T, healthy, target *Bus, iface string) {
	t.Helper()
	var socketErr error
	if err := healthy.raw.Control(func(fd uintptr) {
		socketErr = unix.SetsockoptCanRawFilter(int(fd), unix.SOL_CAN_RAW, unix.CAN_RAW_FILTER, nil)
	}); err != nil {
		t.Fatalf("access healthy receive socket: %v", err)
	}
	if socketErr != nil {
		t.Fatalf("disable healthy socket reception: %v", socketErr)
	}

	socketErr = nil
	if err := target.raw.Control(func(fd uintptr) {
		socketErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 256)
	}); err != nil {
		t.Fatalf("access target receive socket: %v", err)
	}
	if socketErr != nil {
		t.Fatalf("shrink target receive buffer: %v", socketErr)
	}

	sender := openErrorSender(t, iface)
	if err := unix.SetsockoptInt(sender, unix.SOL_CAN_RAW, unix.CAN_RAW_FD_FRAMES, 1); err != nil {
		t.Fatalf("enable CAN FD on sender: %v", err)
	}
	frame, err := gocan.NewFrame(0x123, make([]byte, 64), gocan.FrameFD)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	var buffer [fdMTU]byte
	packet := encodeFrame(frame, &buffer)

	for range 100_000 {
		select {
		case <-target.Done():
			return
		default:
		}
		if _, err := unix.Write(sender, packet); err != nil {
			t.Fatalf("send saturation frame: %v", err)
		}
	}
}
