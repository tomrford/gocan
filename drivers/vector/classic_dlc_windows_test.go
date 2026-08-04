//go:build windows && amd64

package vector

import (
	"context"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/pcan"
)

const classicalDLCHardwareTimeout = 5 * time.Second

func TestVectorClassicDLCAboveEightHardware(t *testing.T) {
	vectorA, vectorB := vectorPairIndexes(t)
	runVectorClassicalDLCMatrix(t, vectorA, vectorB, 0, 0)
}

func TestVectorFDAPIClassicalDLCAboveEightHardware(t *testing.T) {
	vectorA, vectorB := vectorPairIndexes(t)
	dataBitrate := vectorFDDataBitrate(t)
	runVectorClassicalDLCMatrix(t, vectorA, vectorB, dataBitrate, dataBitrate)
}

func TestVectorMixedAPIClassicalDLCAboveEightHardware(t *testing.T) {
	vectorA, vectorB := vectorPairIndexes(t)
	runVectorClassicalDLCMatrix(t, vectorA, vectorB, 0, vectorFDDataBitrate(t))
}

func TestVectorToPCANClassicalDLCAboveEightHardware(t *testing.T) {
	vectorIndex := vectorChannelIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX")
	pcanChannel := pcanPeerChannel(t, "GOCAN_PCAN_CHANNEL")
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

func TestPCANFDAPIClassicalDLCAboveEightHardware(t *testing.T) {
	vectorIndex := vectorChannelIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX")
	pcanChannel := pcanPeerChannel(t, "GOCAN_PCAN_CHANNEL")
	fdBitrate := pcanFDBitrate(t)
	capture := gocan.NewCapture()

	pcanBus, err := pcan.Open(context.Background(), capture, pcan.Config{
		ID:        1,
		Name:      "pcan-fd-classic-dlc",
		Channel:   pcanChannel,
		FDBitrate: fdBitrate,
	})
	if err != nil {
		t.Fatalf("open PCAN FD channel: %v", err)
	}
	t.Cleanup(func() { _ = pcanBus.Close() })

	vectorBus, err := Open(context.Background(), capture, Config{
		ID:           2,
		Name:         "vector-classic-dlc-peer",
		ChannelIndex: vectorIndex,
		Bitrate:      500_000,
	})
	if err != nil {
		_ = pcanBus.Close()
		t.Fatalf("open Vector peer: %v", err)
	}
	t.Cleanup(func() { _ = vectorBus.Close() })

	assertClassicalDLCs(t, capture, pcanBus, vectorBus, 0x640)
	assertClassicalDLCs(t, capture, vectorBus, pcanBus, 0x660)
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
