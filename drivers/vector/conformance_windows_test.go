//go:build windows && amd64

package vector

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
	"github.com/tomrford/gocan/drivers/pcan"
)

func TestVectorConformance(t *testing.T) {
	vectorChannelValue := os.Getenv("GOCAN_VECTOR_CHANNEL_INDEX")
	pcanChannelValue := os.Getenv("GOCAN_PCAN_CHANNEL")
	if vectorChannelValue == "" || pcanChannelValue == "" {
		t.Skip("GOCAN_VECTOR_CHANNEL_INDEX and GOCAN_PCAN_CHANNEL are not set")
	}

	vectorChannel, err := strconv.ParseUint(vectorChannelValue, 0, 8)
	if err != nil || vectorChannel >= 64 {
		t.Fatalf("GOCAN_VECTOR_CHANNEL_INDEX=%q is not a channel index from 0 through 63", vectorChannelValue)
	}
	pcanChannel, err := strconv.ParseUint(pcanChannelValue, 0, 16)
	if err != nil || pcanChannel == 0 {
		t.Fatalf("GOCAN_PCAN_CHANNEL=%q is not a nonzero 16-bit PCAN handle", pcanChannelValue)
	}

	open := func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		sender, err := pcan.Open(context.Background(), capture, pcan.Config{
			ID:      1,
			Name:    "pcan-peer",
			Channel: pcan.Channel(pcanChannel),
			Bitrate: pcan.Bitrate500K,
		})
		if err != nil {
			t.Fatalf("Open PCAN peer: %v", err)
		}
		t.Cleanup(func() { _ = sender.Close() })

		receiver, err := Open(context.Background(), capture, Config{
			ID:           2,
			Name:         "vector",
			ChannelIndex: ChannelIndex(vectorChannel),
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

func vectorFDDataBitrate(t *testing.T) uint32 {
	t.Helper()
	value := os.Getenv("GOCAN_VECTOR_FD_DATA_BITRATE")
	if value == "" {
		t.Skip("GOCAN_VECTOR_FD_DATA_BITRATE is not set")
	}
	dataBitrate, err := strconv.ParseUint(value, 0, 32)
	if err != nil || dataBitrate == 0 {
		t.Fatalf("GOCAN_VECTOR_FD_DATA_BITRATE=%q is not a nonzero 32-bit bitrate", value)
	}
	return uint32(dataBitrate)
}

func vectorPairIndexes(t *testing.T) (ChannelIndex, ChannelIndex) {
	t.Helper()
	vectorAValue := os.Getenv("GOCAN_VECTOR_CHANNEL_INDEX")
	vectorBValue := os.Getenv("GOCAN_VECTOR_CHANNEL_INDEX_B")
	if vectorAValue == "" || vectorBValue == "" {
		t.Skip("GOCAN_VECTOR_CHANNEL_INDEX and GOCAN_VECTOR_CHANNEL_INDEX_B are not set")
	}

	vectorA, err := strconv.ParseUint(vectorAValue, 0, 8)
	if err != nil || vectorA >= 64 {
		t.Fatalf("GOCAN_VECTOR_CHANNEL_INDEX=%q is not a channel index from 0 through 63", vectorAValue)
	}
	vectorB, err := strconv.ParseUint(vectorBValue, 0, 8)
	if err != nil || vectorB >= 64 || vectorB == vectorA {
		t.Fatalf("GOCAN_VECTOR_CHANNEL_INDEX_B=%q is not a distinct channel index from 0 through 63", vectorBValue)
	}
	return ChannelIndex(vectorA), ChannelIndex(vectorB)
}

func openVectorPair(vectorA, vectorB ChannelIndex, dataBitrate uint32) conformance.Pair {
	return func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		first, err := Open(context.Background(), capture, Config{
			ID:           1,
			Name:         "vector-a",
			ChannelIndex: vectorA,
			Bitrate:      500_000,
			DataBitrate:  dataBitrate,
		})
		if err != nil {
			t.Fatalf("Open first Vector channel: %v", err)
		}
		t.Cleanup(func() { _ = first.Close() })

		second, err := Open(context.Background(), capture, Config{
			ID:           2,
			Name:         "vector-b",
			ChannelIndex: vectorB,
			Bitrate:      500_000,
			DataBitrate:  dataBitrate,
		})
		if err != nil {
			_ = first.Close()
			t.Fatalf("Open second Vector channel: %v", err)
		}
		t.Cleanup(func() { _ = second.Close() })
		return first, second
	}
}
