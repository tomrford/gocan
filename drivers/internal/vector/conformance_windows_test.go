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
	runVectorConformance(t, false)
}

func TestVectorJoinedConformance(t *testing.T) {
	runVectorConformance(t, true)
}

func runVectorConformance(t *testing.T, join bool) {
	t.Helper()
	vectorChannel := vectorChannelIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX")
	pcanChannel := pcanPeerChannel(t, "GOCAN_PCAN_CHANNEL")
	config := Config{ID: 2, Name: "vector", ChannelIndex: vectorChannel, Bitrate: 500_000}
	if join {
		holdVectorOwner(t, config)
		config.Bitrate, config.Join = 0, true
	}

	open := func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		sender, err := pcan.Open(context.Background(), capture, pcan.Config{
			ID:      1,
			Name:    "pcan-peer",
			Channel: pcanChannel,
			Bitrate: 500_000,
		})
		if err != nil {
			t.Fatalf("Open PCAN peer: %v", err)
		}
		t.Cleanup(func() { _ = sender.Close() })

		receiver, err := Open(context.Background(), capture, config)
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
	conformance.Run(t, openVectorPair(vectorA, vectorB, 0, false), conformance.Capabilities{RemoteFrames: true})
}

func TestVectorFDConformance(t *testing.T) {
	runVectorFDConformance(t, false)
}

func TestVectorJoinedFDConformance(t *testing.T) {
	runVectorFDConformance(t, true)
}

func runVectorFDConformance(t *testing.T, join bool) {
	t.Helper()
	vectorA, vectorB := vectorPairIndexes(t)
	dataBitrate := vectorFDDataBitrate(t)
	if join {
		holdVectorOwner(t, Config{ID: 9, Name: "vector-owner", ChannelIndex: vectorA, FDTiming: vectorFDTiming(dataBitrate)})
	}
	conformance.Run(t, openVectorPair(vectorA, vectorB, dataBitrate, join), conformance.Capabilities{
		FD:           true,
		RemoteFrames: true,
	})
}

func openVectorPair(vectorA, vectorB ChannelIndex, dataBitrate uint32, join bool) conformance.Pair {
	return func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		configA := Config{
			ID:           1,
			Name:         "vector-a",
			ChannelIndex: vectorA,
		}
		if join {
			configA.Join = true
		} else if dataBitrate == 0 {
			configA.Bitrate = 500_000
		} else {
			configA.FDTiming = vectorFDTiming(dataBitrate)
		}
		first, err := Open(context.Background(), capture, configA)
		if err != nil {
			t.Fatalf("Open first Vector channel: %v", err)
		}
		t.Cleanup(func() { _ = first.Close() })

		configB := Config{
			ID:           2,
			Name:         "vector-b",
			ChannelIndex: vectorB,
		}
		if dataBitrate == 0 {
			configB.Bitrate = 500_000
		} else {
			configB.FDTiming = vectorFDTiming(dataBitrate)
		}
		second, err := Open(context.Background(), capture, configB)
		if err != nil {
			_ = first.Close()
			t.Fatalf("Open second Vector channel: %v", err)
		}
		t.Cleanup(func() { _ = second.Close() })
		return first, second
	}
}

// Keep the configuring port alive while conformance repeatedly opens and closes
// joined ports, as a test application would during flashing.
func holdVectorOwner(t *testing.T, config Config) {
	t.Helper()
	owner, err := Open(context.Background(), gocan.NewCapture(), config)
	if err != nil {
		t.Fatalf("Open owning Vector port: %v", err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Errorf("close owning Vector port: %v", err)
		}
	})
}
