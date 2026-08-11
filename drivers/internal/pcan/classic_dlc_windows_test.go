//go:build windows

package pcan

import (
	"context"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

func TestPCANClassicAPIRejectsDLCAboveEightHardware(t *testing.T) {
	channelA := testChannel(t, "GOCAN_PCAN_CHANNEL_A")
	channelB := testChannel(t, "GOCAN_PCAN_CHANNEL_B")
	if channelA == channelB {
		t.Fatalf("GOCAN_PCAN_CHANNEL_A and GOCAN_PCAN_CHANNEL_B both select channel %#x", uint16(channelA))
	}
	capture := gocan.NewCapture()
	a, err := Open(context.Background(), capture, Config{
		ID: 1, Name: "pcan-classic-dlc-a", Channel: channelA, Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("open PCAN A: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := Open(context.Background(), capture, Config{
		ID: 2, Name: "pcan-classic-dlc-b", Channel: channelB, Bitrate: 500_000,
	})
	if err != nil {
		_ = a.Close()
		t.Fatalf("open PCAN B: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	rejected := gocan.Frame{ID: 0x6e0, DLC: 9}
	copy(rejected.Data[:8], []byte{9, 1, 2, 3, 4, 5, 6, 7})
	if err := a.Send(context.Background(), rejected); err == nil {
		t.Fatal("classical API accepted DLC 9")
	}

	accepted, err := gocan.NewFrame(0x6e1, []byte{8, 1, 2, 3, 4, 5, 6, 7}, 0)
	if err != nil {
		t.Fatalf("build DLC 8 recovery frame: %v", err)
	}
	cursor := capture.End()
	if err := a.Send(context.Background(), accepted); err != nil {
		t.Fatalf("send DLC 8 after rejection: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	event, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus: b.ID(), ID: accepted.ID, Direction: gocan.DirectionReceive,
	}, cursor)
	if err != nil {
		t.Fatalf("receive DLC 8 after rejection: %v", err)
	}
	if event.Frame != accepted {
		t.Fatalf("received DLC 8 frame = %+v, want %+v", event.Frame, accepted)
	}
}
