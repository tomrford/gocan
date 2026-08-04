//go:build windows

package pcan

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

func TestPCANClassicAPIRejectsDLCAboveEightHardware(t *testing.T) {
	channelA := currentPCANTestChannel(t, "GOCAN_PCAN_CHANNEL_A")
	channelB := currentPCANTestChannel(t, "GOCAN_PCAN_CHANNEL_B")
	if channelA == channelB {
		t.Fatalf("GOCAN_PCAN_CHANNEL_A and GOCAN_PCAN_CHANNEL_B both select channel %#x", uint16(channelA))
	}
	capture := gocan.NewCapture()
	a, err := Open(context.Background(), capture, Config{
		ID: 1, Name: "pcan-classic-dlc-a", Channel: channelA, Bitrate: Bitrate500K,
	})
	if err != nil {
		t.Fatalf("open PCAN A: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := Open(context.Background(), capture, Config{
		ID: 2, Name: "pcan-classic-dlc-b", Channel: channelB, Bitrate: Bitrate500K,
	})
	if err != nil {
		_ = a.Close()
		t.Fatalf("open PCAN B: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	rejected := gocan.Frame{ID: 0x6e0, DLC: 9}
	copy(rejected.Data[:8], []byte{9, 1, 2, 3, 4, 5, 6, 7})
	if err := a.Send(context.Background(), rejected); err == nil ||
		!strings.Contains(err.Error(), "PCAN classical API cannot send DLC 9") {
		t.Fatalf("send classical DLC 9 = %v, want path-specific rejection", err)
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

func currentPCANTestChannel(t *testing.T, envName string) Channel {
	t.Helper()
	value := os.Getenv(envName)
	if value == "" {
		t.Skipf("%s is not set", envName)
	}
	parsed, err := strconv.ParseUint(value, 0, 16)
	if err != nil || parsed == 0 {
		t.Fatalf("%s=%q is not a nonzero 16-bit PCAN handle", envName, value)
	}
	want := Channel(parsed)
	channels, err := Discover()
	if err != nil {
		t.Fatalf("rediscover PCAN channels: %v", err)
	}
	for _, channel := range channels {
		if channel.Channel == want {
			return want
		}
	}
	t.Fatalf("PCAN channel %#x from %s is absent from the current discovery result: %+v", uint16(want), envName, channels)
	return 0
}
