//go:build linux

package socketcan

import (
	"context"
	"os"
	"slices"
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
		FD:                     true,
		RemoteFrames:           true,
		ClassicalDLCAboveEight: true,
		InduceOverrun: func(t *testing.T, healthy, target gocan.Bus) {
			induceVCanOverrun(t, healthy.(*Bus), target.(*Bus), iface)
		},
		// Synthetic error packets reach both endpoints; vcan has no controller
		// to reject frames during bus-off.
		InduceBusOff: func(t *testing.T, _ gocan.Bus) func() {
			sender := openErrorSender(t, iface)
			sendErrorPacket(t, sender, unix.CAN_ERR_BUSOFF, [8]byte{})
			return func() { sendErrorPacket(t, sender, unix.CAN_ERR_RESTARTED, [8]byte{}) }
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

	for sent := range 100_000 {
		if sent%64 == 0 {
			events, _, _ := target.capture.BusEvents(target.id, gocan.Range{})
			if slices.ContainsFunc(slices.Collect(events), func(event gocan.Event) bool {
				return event.Kind == gocan.EventReceiveOverrun
			}) {
				return
			}
		}
		if _, err := unix.Write(sender, packet); err != nil {
			t.Fatalf("send saturation frame: %v", err)
		}
	}
	t.Fatal("target reported no overrun")
}
