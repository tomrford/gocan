//go:build linux

package socketcan

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

// TestVCanClassicalDLCAboveEight sends classical data and remote frames with
// DLC 9-15 between two vcan sockets. vcan does not rewrite frames, so the raw
// DLC must survive the len8_dlc encoding in both directions.
func TestVCanClassicalDLCAboveEight(t *testing.T) {
	iface := os.Getenv("GOCAN_VCAN_INTERFACE")
	if iface == "" {
		t.Skip("GOCAN_VCAN_INTERFACE is not set")
	}

	capture := gocan.NewCapture()
	sender, err := Open(context.Background(), capture, Config{ID: 1, Name: "dlc-sender", Interface: iface, FD: true})
	if err != nil {
		t.Fatalf("Open sender: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	receiver, err := Open(context.Background(), capture, Config{ID: 2, Name: "dlc-receiver", Interface: iface, FD: true})
	if err != nil {
		t.Fatalf("Open receiver: %v", err)
	}
	t.Cleanup(func() { _ = receiver.Close() })

	var frames []gocan.Frame
	for _, dlc := range []uint8{9, 15} {
		data := gocan.Frame{ID: 0x6c0 + uint32(dlc), DLC: dlc}
		copy(data.Data[:8], []byte{1, 2, 3, 4, 5, 6, 7, 8})
		remote, err := gocan.NewRemoteFrame(0x1abc0+uint32(dlc), dlc, true)
		if err != nil {
			t.Fatalf("NewRemoteFrame DLC %d: %v", dlc, err)
		}
		frames = append(frames, data, remote)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, frame := range frames {
		cursor := capture.End()
		if err := sender.Send(ctx, frame); err != nil {
			t.Fatalf("Send %v: %v", frame, err)
		}
		for _, side := range []struct {
			bus       gocan.Bus
			direction gocan.Direction
		}{{sender, gocan.DirectionTransmit}, {receiver, gocan.DirectionReceive}} {
			key := gocan.FrameKey{Bus: side.bus.ID(), ID: frame.ID, Direction: side.direction, Extended: frame.Flags.Has(gocan.FrameExtended)}
			event, _, err := capture.Next(ctx, key, cursor)
			if err != nil {
				t.Fatalf("waiting for %v %v: %v", side.direction, frame, err)
			}
			if event.Frame != frame {
				t.Fatalf("%v frame = %v, want %v", side.direction, event.Frame, frame)
			}
		}
	}
}
