//go:build windows && amd64

package vector

import (
	"os"
	"strconv"
	"testing"

	"github.com/tomrford/gocan/drivers/internal/pcan"
)

func parseHardwareUint(tb testing.TB, name, value string, bits int) uint64 {
	tb.Helper()
	parsed, err := strconv.ParseUint(value, 0, bits)
	if err != nil {
		tb.Fatalf("%s=%q is not an unsigned %d-bit integer", name, value, bits)
	}
	return parsed
}

// vectorChannelIndex returns the Vector channel index selected by the named
// environment variable, skipping the test when the variable is not set.
func vectorChannelIndex(tb testing.TB, name string) ChannelIndex {
	tb.Helper()
	value := os.Getenv(name)
	if value == "" {
		tb.Skipf("%s is not set", name)
	}
	index := parseHardwareUint(tb, name, value, 8)
	if index >= 64 {
		tb.Fatalf("%s=%q is not a channel index from 0 through 63", name, value)
	}
	return ChannelIndex(index)
}

// vectorPairIndexes returns the two distinct Vector channel indexes selected
// by GOCAN_VECTOR_CHANNEL_INDEX and GOCAN_VECTOR_CHANNEL_INDEX_B.
func vectorPairIndexes(tb testing.TB) (ChannelIndex, ChannelIndex) {
	tb.Helper()
	vectorA := vectorChannelIndex(tb, "GOCAN_VECTOR_CHANNEL_INDEX")
	vectorB := vectorChannelIndex(tb, "GOCAN_VECTOR_CHANNEL_INDEX_B")
	if vectorA == vectorB {
		tb.Fatalf("GOCAN_VECTOR_CHANNEL_INDEX and GOCAN_VECTOR_CHANNEL_INDEX_B both select channel %d", vectorA)
	}
	return vectorA, vectorB
}

// pcanPeerChannel returns the PCAN channel handle selected by the named
// environment variable, skipping the test when the variable is not set.
func pcanPeerChannel(tb testing.TB, name string) pcan.Channel {
	tb.Helper()
	value := os.Getenv(name)
	if value == "" {
		tb.Skipf("%s is not set", name)
	}
	channel := parseHardwareUint(tb, name, value, 16)
	if channel == 0 {
		tb.Fatalf("%s=%q is not a nonzero 16-bit PCAN handle", name, value)
	}
	return pcan.Channel(channel)
}

// pcanFDBitrate returns the PCAN-Basic CAN FD timing string selected by
// GOCAN_PCAN_FD_BITRATE, skipping the test when it is not set.
func pcanFDBitrate(tb testing.TB) string {
	tb.Helper()
	value := os.Getenv("GOCAN_PCAN_FD_BITRATE")
	if value == "" {
		tb.Skip("GOCAN_PCAN_FD_BITRATE is not set")
	}
	return value
}

// vectorFDDataBitrate returns the CAN FD data bitrate selected by
// GOCAN_VECTOR_FD_DATA_BITRATE, skipping the test when it is not set.
func vectorFDDataBitrate(tb testing.TB) uint32 {
	tb.Helper()
	value := os.Getenv("GOCAN_VECTOR_FD_DATA_BITRATE")
	if value == "" {
		tb.Skip("GOCAN_VECTOR_FD_DATA_BITRATE is not set")
	}
	dataBitrate := parseHardwareUint(tb, "GOCAN_VECTOR_FD_DATA_BITRATE", value, 32)
	if dataBitrate == 0 {
		tb.Fatal("GOCAN_VECTOR_FD_DATA_BITRATE must be nonzero")
	}
	return uint32(dataBitrate)
}

func vectorFDTiming(dataBitrate uint32) FDTiming {
	return FDTiming{
		ArbitrationBitrate: 500_000,
		DataBitrate:        dataBitrate,
		Arbitration:        BitTiming{SJW: 2, TSEG1: 6, TSEG2: 3},
		Data:               BitTiming{SJW: 2, TSEG1: 6, TSEG2: 3},
	}
}
