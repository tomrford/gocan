//go:build windows

package zlg

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
	"github.com/tomrford/gocan/drivers/internal/pcan"
)

// The hardware tests pair a ZLG channel with a PCAN reference adapter on one
// terminated 500 kbit/s bus. GOCAN_ZLG_DEVICE selects the ZLG device index
// (channel 0) and GOCAN_PCAN_CHANNEL the PCAN handle; GOCAN_ZLGCAN_DIR must
// hold zlgcan.dll.
func hardwarePair(t testing.TB) (Config, pcan.Config) {
	t.Helper()
	device, peer := os.Getenv("GOCAN_ZLG_DEVICE"), os.Getenv("GOCAN_PCAN_CHANNEL")
	if device == "" || peer == "" {
		t.Skip("GOCAN_ZLG_DEVICE and GOCAN_PCAN_CHANNEL are not set")
	}
	index, err := strconv.ParseUint(device, 0, 32)
	if err != nil {
		t.Fatalf("GOCAN_ZLG_DEVICE=%q: %v", device, err)
	}
	channel, err := strconv.ParseUint(peer, 0, 16)
	if err != nil || channel == 0 {
		t.Fatalf("GOCAN_PCAN_CHANNEL=%q is not a nonzero PCAN handle", peer)
	}
	return Config{ID: 1, Name: "zlg", Device: uint32(index), Bitrate: 500_000},
		pcan.Config{ID: 2, Name: "pcan", Channel: pcan.Channel(channel), Bitrate: 500_000}
}

func TestZLGConformance(t *testing.T) {
	zlgConfig, pcanConfig := hardwarePair(t)
	open := func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		a, err := Open(context.Background(), capture, zlgConfig)
		if err != nil {
			t.Fatalf("open ZLG: %v", err)
		}
		t.Cleanup(func() { _ = a.Close() })
		b, err := pcan.Open(context.Background(), capture, pcanConfig)
		if err != nil {
			t.Fatalf("open PCAN: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return a, b
	}
	conformance.Run(t, open, conformance.Capabilities{RemoteFrames: true})
}

// Send returns once a frame is queued, so Close must transmit every frame
// already accepted before releasing the channel.
func TestZLGCloseTransmitsQueuedFramesHardware(t *testing.T) {
	zlgConfig, pcanConfig := hardwarePair(t)
	capture := gocan.NewCapture()
	peer, err := pcan.Open(context.Background(), capture, pcanConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	bus, err := Open(context.Background(), capture, zlgConfig)
	if err != nil {
		t.Fatal(err)
	}
	const queued = transmitQueueFrames
	for sequence := range queued {
		frame, err := gocan.NewFrame(0x540, []byte{byte(sequence >> 8), byte(sequence)}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := gocan.Send(context.Background(), bus, frame, time.Second); err != nil {
			t.Fatalf("Send %d: %v", sequence, err)
		}
	}
	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var cursor gocan.Cursor
	for sequence := range queued {
		event, next, err := capture.Next(ctx, gocan.FrameKey{Bus: peer.ID(), ID: 0x540, Direction: gocan.DirectionReceive}, cursor)
		if err != nil {
			t.Fatalf("peer received %d of %d queued frames: %v", sequence, queued, err)
		}
		if got := int(event.Frame.Data[0])<<8 | int(event.Frame.Data[1]); got != sequence {
			t.Fatalf("frame %d arrived as sequence %d", sequence, got)
		}
		cursor = next
	}
}

func TestZLGUnknownChannelHardware(t *testing.T) {
	zlgConfig, _ := hardwarePair(t)
	zlgConfig.Channel = 7
	if _, err := Open(context.Background(), gocan.NewCapture(), zlgConfig); err == nil {
		t.Fatal("opened a channel the device lacks")
	}
	// The failed open must release the device for the next one.
	zlgConfig.Channel = 0
	bus, err := Open(context.Background(), gocan.NewCapture(), zlgConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}
}
