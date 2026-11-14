//go:build windows

package zlg

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
	"github.com/tomrford/gocan/drivers/internal/pcan"
)

// The hardware tests pair a ZLG channel with a PCAN reference adapter on one
// terminated 500 kbit/s classical bus. GOCAN_ZLG_DEVICE selects the ZLG device
// type and index (channel 0), for example 4:0 for a USBCAN-I or 41:0 for a
// USBCANFD-200U, and GOCAN_PCAN_CHANNEL the PCAN handle.
func hardwarePair(t testing.TB) (Config, pcan.Config) {
	t.Helper()
	device, peer := os.Getenv("GOCAN_ZLG_DEVICE"), os.Getenv("GOCAN_PCAN_CHANNEL")
	if device == "" || peer == "" {
		t.Skip("GOCAN_ZLG_DEVICE and GOCAN_PCAN_CHANNEL are not set")
	}
	deviceType, index, found := strings.Cut(device, ":")
	parsedType, typeErr := strconv.ParseUint(deviceType, 0, 32)
	parsedIndex, indexErr := strconv.ParseUint(index, 0, 16)
	if !found || typeErr != nil || indexErr != nil {
		t.Fatalf("GOCAN_ZLG_DEVICE=%q is not a device type and index such as 4:0", device)
	}
	channel, err := strconv.ParseUint(peer, 0, 16)
	if err != nil || channel == 0 {
		t.Fatalf("GOCAN_PCAN_CHANNEL=%q is not a nonzero PCAN handle", peer)
	}
	return Config{ID: 1, Name: "zlg", DeviceType: uint32(parsedType), Device: uint32(parsedIndex), Bitrate: 500_000},
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

// The soak repeats a full lifecycle until GOCAN_ZLG_SOAK (a duration such as
// 10m) elapses: open both adapters, exchange sequenced frames in both
// directions at once, then close. Every frame must arrive once and in order,
// with no error frames, overruns, or controller state other than active.
func TestZLGSoakHardware(t *testing.T) {
	zlgConfig, pcanConfig := hardwarePair(t)
	setting := os.Getenv("GOCAN_ZLG_SOAK")
	if setting == "" {
		t.Skip("GOCAN_ZLG_SOAK is not set")
	}
	duration, err := time.ParseDuration(setting)
	if err != nil {
		t.Fatalf("GOCAN_ZLG_SOAK=%q: %v", setting, err)
	}
	const frames = 1000
	deadline := time.Now().Add(duration)
	cycles := 0
	for ; time.Now().Before(deadline); cycles++ {
		capture := gocan.NewCapture()
		peer, err := pcan.Open(context.Background(), capture, pcanConfig)
		if err != nil {
			t.Fatalf("cycle %d: open PCAN: %v", cycles, err)
		}
		bus, err := Open(context.Background(), capture, zlgConfig)
		if err != nil {
			_ = peer.Close()
			t.Fatalf("cycle %d: open ZLG: %v", cycles, err)
		}
		sent := make(chan error, 2)
		for _, sender := range []struct {
			bus gocan.Bus
			id  uint32
		}{{bus, 0x5a0}, {peer, 0x5a1}} {
			go func() {
				for sequence := range frames {
					frame, err := gocan.NewFrame(sender.id, []byte{byte(sequence >> 8), byte(sequence)}, 0)
					if err == nil {
						err = gocan.Send(context.Background(), sender.bus, frame, time.Second)
					}
					if err != nil {
						sent <- err
						return
					}
				}
				sent <- nil
			}()
		}
		for range 2 {
			if err := <-sent; err != nil {
				t.Fatalf("cycle %d: send: %v", cycles, err)
			}
		}
		expectSequence(t, cycles, capture, peer.ID(), 0x5a0, frames)
		expectSequence(t, cycles, capture, bus.ID(), 0x5a1, frames)
		if err := bus.Close(); err != nil {
			t.Fatalf("cycle %d: close ZLG: %v", cycles, err)
		}
		if err := peer.Close(); err != nil {
			t.Fatalf("cycle %d: close PCAN: %v", cycles, err)
		}
		for _, event := range capture.Events() {
			if event.Kind != gocan.EventControllerState || event.ControllerState != gocan.ControllerActive {
				t.Fatalf("cycle %d: bus %d reported %+v", cycles, event.Bus, event)
			}
		}
	}
	t.Logf("%d cycles of %d frames each way in %v", cycles, frames, duration)
}

func expectSequence(t *testing.T, cycle int, capture *gocan.Capture, bus gocan.BusID, id uint32, frames int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cursor gocan.Cursor
	for sequence := range frames {
		event, next, err := capture.Next(ctx, gocan.FrameKey{Bus: bus, ID: id, Direction: gocan.DirectionReceive}, cursor)
		if err != nil {
			t.Fatalf("cycle %d: bus %d received %d of %d frames %#x: %v; events %+v", cycle, bus, sequence, frames, id, err, capture.Events())
		}
		if got := int(event.Frame.Data[0])<<8 | int(event.Frame.Data[1]); got != sequence {
			t.Fatalf("cycle %d: bus %d frame %d of %#x arrived as sequence %d; events %+v", cycle, bus, sequence, id, got, capture.Events())
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
