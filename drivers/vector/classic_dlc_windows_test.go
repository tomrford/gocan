//go:build windows && amd64

package vector

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/pcan"
)

const classicalDLCHardwareTimeout = 5 * time.Second

func TestVectorClassicDLCAboveEightHardware(t *testing.T) {
	vectorA, vectorB := currentVectorPair(t, false)
	runVectorClassicalDLCMatrix(t, vectorA, vectorB, 0, 0)
}

func TestVectorFDAPIClassicalDLCAboveEightHardware(t *testing.T) {
	vectorA, vectorB := currentVectorPair(t, true)
	dataBitrate := vectorFDDataBitrate(t)
	runVectorClassicalDLCMatrix(t, vectorA, vectorB, dataBitrate, dataBitrate)
}

func TestVectorMixedAPIClassicalDLCAboveEightHardware(t *testing.T) {
	vectorA, vectorB := currentVectorPair(t, true)
	runVectorClassicalDLCMatrix(t, vectorA, vectorB, 0, vectorFDDataBitrate(t))
}

func TestVectorToPCANClassicalDLCAboveEightHardware(t *testing.T) {
	vectorIndex := currentVectorIndex(t, "GOCAN_VECTOR_CLASSIC_DLC_CHANNEL_INDEX", false)
	pcanChannel := currentPCANChannel(t, "GOCAN_PCAN_CLASSIC_DLC_CHANNEL")
	capture := gocan.NewCapture()

	sender, err := Open(context.Background(), capture, Config{
		ID:           1,
		Name:         "vector-classic-dlc-sender",
		ChannelIndex: vectorIndex,
		Bitrate:      500_000,
	})
	if err != nil {
		t.Fatalf("open Vector sender: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	receiver, err := pcan.Open(context.Background(), capture, pcan.Config{
		ID:      2,
		Name:    "pcan-classic-dlc-receiver",
		Channel: pcanChannel,
		Bitrate: pcan.Bitrate500K,
	})
	if err != nil {
		_ = sender.Close()
		t.Fatalf("open PCAN receiver: %v", err)
	}
	t.Cleanup(func() { _ = receiver.Close() })

	assertClassicalDLCs(t, capture, sender, receiver, 0x6c0)
}

func runVectorClassicalDLCMatrix(
	t *testing.T,
	vectorA, vectorB ChannelIndex,
	dataBitrateA, dataBitrateB uint32,
) {
	t.Helper()
	capture := gocan.NewCapture()
	a, err := Open(context.Background(), capture, Config{
		ID:           1,
		Name:         "vector-dlc-a",
		ChannelIndex: vectorA,
		Bitrate:      500_000,
		DataBitrate:  dataBitrateA,
	})
	if err != nil {
		t.Fatalf("open Vector A: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := Open(context.Background(), capture, Config{
		ID:           2,
		Name:         "vector-dlc-b",
		ChannelIndex: vectorB,
		Bitrate:      500_000,
		DataBitrate:  dataBitrateB,
	})
	if err != nil {
		_ = a.Close()
		t.Fatalf("open Vector B: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	assertClassicalDLCs(t, capture, a, b, 0x600)
	assertClassicalDLCs(t, capture, b, a, 0x620)
}

func assertClassicalDLCs(
	t *testing.T,
	capture *gocan.Capture,
	sender, receiver gocan.Bus,
	baseID uint32,
) {
	t.Helper()
	for _, remote := range []bool{false, true} {
		for dlc := uint8(9); dlc <= 15; dlc++ {
			flags := gocan.FrameFlags(0)
			kindOffset := uint32(0)
			if remote {
				flags = gocan.FrameRemote
				kindOffset = 8
			}
			frame := gocan.Frame{
				ID:    baseID + kindOffset + uint32(dlc-9),
				DLC:   dlc,
				Flags: flags,
			}
			if !remote {
				copy(frame.Data[:8], []byte{dlc, 1, 2, 3, 4, 5, 6, 7})
			}
			if err := frame.Validate(); err != nil {
				t.Fatalf("validate DLC %d fixture: %v", dlc, err)
			}

			cursor := capture.End()
			if err := sender.Send(context.Background(), frame); err != nil {
				t.Fatalf("send DLC %d remote=%t from %s: %v", dlc, remote, sender.Name(), err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), classicalDLCHardwareTimeout)
			event, _, err := capture.Next(ctx, gocan.FrameKey{
				Bus:       receiver.ID(),
				ID:        frame.ID,
				Direction: gocan.DirectionReceive,
			}, cursor)
			cancel()
			if err != nil {
				t.Fatalf("receive DLC %d remote=%t from %s on %s: %v", dlc, remote, sender.Name(), receiver.Name(), err)
			}
			if event.Frame != frame {
				t.Fatalf("receive DLC %d remote=%t from %s on %s = %+v, want %+v", dlc, remote, sender.Name(), receiver.Name(), event.Frame, frame)
			}
		}
	}
}

func currentVectorPair(t *testing.T, requireFD bool) (ChannelIndex, ChannelIndex) {
	t.Helper()
	a := currentVectorIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX", requireFD)
	b := currentVectorIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX_B", requireFD)
	if a == b {
		t.Fatalf("GOCAN_VECTOR_CHANNEL_INDEX and GOCAN_VECTOR_CHANNEL_INDEX_B both select channel %d", a)
	}
	return a, b
}

func currentVectorIndex(t *testing.T, envName string, requireFD bool) ChannelIndex {
	t.Helper()
	value := os.Getenv(envName)
	if value == "" {
		t.Skipf("%s is not set", envName)
	}
	parsed, err := strconv.ParseUint(value, 0, 8)
	if err != nil || parsed >= 64 {
		t.Fatalf("%s=%q is not a channel index from 0 through 63", envName, value)
	}
	want := ChannelIndex(parsed)
	channels, err := Discover()
	if err != nil {
		t.Fatalf("rediscover Vector channels: %v", err)
	}
	for _, channel := range channels {
		if channel.ChannelIndex != want {
			continue
		}
		if requireFD && !channel.SupportsFD {
			t.Fatalf("rediscovered Vector channel %d (%s) is not FD-capable", want, channel.Name)
		}
		return want
	}
	t.Fatalf("Vector channel index %d from %s is absent from the current discovery result: %+v", want, envName, channels)
	return 0
}

func currentPCANChannel(t *testing.T, envName string) pcan.Channel {
	t.Helper()
	value := os.Getenv(envName)
	if value == "" {
		t.Skipf("%s is not set", envName)
	}
	parsed, err := strconv.ParseUint(value, 0, 16)
	if err != nil || parsed == 0 {
		t.Fatalf("%s=%q is not a nonzero 16-bit PCAN handle", envName, value)
	}
	want := pcan.Channel(parsed)
	channels, err := pcan.Discover()
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
