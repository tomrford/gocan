//go:build windows

package drivers

import (
	"os"
	"strconv"
	"testing"

	"github.com/tomrford/gocan/drivers/pcan"
	"github.com/tomrford/gocan/drivers/vector"
)

func TestDiscoverWindowsHardware(t *testing.T) {
	pcanValue := os.Getenv("GOCAN_PCAN_CHANNEL")
	vectorValue := os.Getenv("GOCAN_VECTOR_CHANNEL_INDEX")
	if pcanValue == "" || vectorValue == "" {
		t.Skip("GOCAN_PCAN_CHANNEL and GOCAN_VECTOR_CHANNEL_INDEX are not set")
	}
	pcanChannel, err := strconv.ParseUint(pcanValue, 0, 16)
	if err != nil || pcanChannel == 0 {
		t.Fatalf("GOCAN_PCAN_CHANNEL=%q is not a nonzero 16-bit PCAN handle", pcanValue)
	}
	vectorIndex, err := strconv.ParseUint(vectorValue, 0, 8)
	if err != nil || vectorIndex >= 64 {
		t.Fatalf("GOCAN_VECTOR_CHANNEL_INDEX=%q is not a channel index from 0 through 63", vectorValue)
	}

	channels, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	foundPCAN := false
	foundVector := false
	for _, channel := range channels {
		if channel.Name == "" {
			t.Errorf("%s channel %+v has an empty name", channel.Driver, channel)
		}
		switch {
		case channel.Driver == "pcan" && channel.PCANChannel == pcan.Channel(pcanChannel):
			foundPCAN = true
		case channel.Driver == "vector" && channel.VectorChannelIndex == vector.ChannelIndex(vectorIndex):
			foundVector = true
		}
	}
	if !foundPCAN || !foundVector {
		t.Fatalf(
			"PCAN %#x found=%t and Vector index %d found=%t in %+v",
			pcanChannel, foundPCAN, vectorIndex, foundVector, channels,
		)
	}
}
