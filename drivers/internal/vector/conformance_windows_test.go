//go:build windows && amd64

package vector

import (
	"context"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
	"github.com/tomrford/gocan/drivers/internal/pcan"
)

func TestVectorConformance(t *testing.T) {
	vectorChannel := vectorChannelIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX")
	pcanChannel := pcanPeerChannel(t, "GOCAN_PCAN_CHANNEL")

	open := func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		sender, err := pcan.Open(context.Background(), capture, pcan.Config{
			ID:      1,
			Name:    "pcan-peer",
			Channel: pcanChannel,
			Bitrate: pcan.Bitrate500K,
		})
		if err != nil {
			t.Fatalf("Open PCAN peer: %v", err)
		}
		t.Cleanup(func() { _ = sender.Close() })

		receiver, err := Open(context.Background(), capture, Config{
			ID:           2,
			Name:         "vector",
			ChannelIndex: vectorChannel,
			Bitrate:      500_000,
		})
		if err != nil {
			_ = sender.Close()
			t.Fatalf("Open Vector: %v", err)
		}
		t.Cleanup(func() { _ = receiver.Close() })
		return sender, receiver
	}

	conformance.Run(t, open, conformance.Capabilities{RemoteFrames: true})
}

func TestVectorPCANFDConformance(t *testing.T) {
	vectorChannel := vectorChannelIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX")
	pcanChannel := pcanPeerChannel(t, "GOCAN_PCAN_CHANNEL")
	fdBitrate := pcanFDBitrate(t)
	dataBitrate := vectorFDDataBitrate(t)

	open := func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		sender, err := pcan.Open(context.Background(), capture, pcan.Config{
			ID:        1,
			Name:      "pcan-fd-peer",
			Channel:   pcanChannel,
			FDBitrate: fdBitrate,
		})
		if err != nil {
			t.Fatalf("Open PCAN FD peer: %v", err)
		}
		t.Cleanup(func() { _ = sender.Close() })

		receiver, err := Open(context.Background(), capture, Config{
			ID:           2,
			Name:         "vector-fd",
			ChannelIndex: vectorChannel,
			FDTiming:     vectorFDTiming(dataBitrate),
		})
		if err != nil {
			_ = sender.Close()
			t.Fatalf("Open Vector FD: %v", err)
		}
		t.Cleanup(func() { _ = receiver.Close() })
		return sender, receiver
	}

	conformance.Run(t, open, conformance.Capabilities{
		FD:           true,
		RemoteFrames: true,
	})
}

func TestVectorDualChannelConformance(t *testing.T) {
	vectorA, vectorB := vectorPairIndexes(t)
	conformance.Run(t, openVectorPair(vectorA, vectorB, 0), conformance.Capabilities{RemoteFrames: true})
}

func TestVectorFDConformance(t *testing.T) {
	vectorA, vectorB := vectorPairIndexes(t)
	conformance.Run(t, openVectorPair(vectorA, vectorB, vectorFDDataBitrate(t)), conformance.Capabilities{
		FD:           true,
		RemoteFrames: true,
	})
}

func openVectorPair(vectorA, vectorB ChannelIndex, dataBitrate uint32) conformance.Pair {
	return func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		first, err := Open(context.Background(), capture, Config{
			ID:           1,
			Name:         "vector-a",
			ChannelIndex: vectorA,
			FDTiming:     vectorFDTiming(dataBitrate),
		})
		if err != nil {
			t.Fatalf("Open first Vector channel: %v", err)
		}
		t.Cleanup(func() { _ = first.Close() })

		second, err := Open(context.Background(), capture, Config{
			ID:           2,
			Name:         "vector-b",
			ChannelIndex: vectorB,
			FDTiming:     vectorFDTiming(dataBitrate),
		})
		if err != nil {
			_ = first.Close()
			t.Fatalf("Open second Vector channel: %v", err)
		}
		t.Cleanup(func() { _ = second.Close() })
		return first, second
	}
}
