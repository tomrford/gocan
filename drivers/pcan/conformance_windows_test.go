//go:build windows

package pcan

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
)

func TestPCANConformance(t *testing.T) {
	channelAValue := os.Getenv("GOCAN_PCAN_CHANNEL_A")
	channelBValue := os.Getenv("GOCAN_PCAN_CHANNEL_B")
	if channelAValue == "" || channelBValue == "" {
		t.Skip("GOCAN_PCAN_CHANNEL_A and GOCAN_PCAN_CHANNEL_B are not set")
	}

	parseChannel := func(name, value string) Channel {
		t.Helper()
		channel, err := strconv.ParseUint(value, 0, 16)
		if err != nil || channel == 0 {
			t.Fatalf("%s=%q is not a nonzero 16-bit numeric PCAN handle", name, value)
		}
		return Channel(channel)
	}
	channelA := parseChannel("GOCAN_PCAN_CHANNEL_A", channelAValue)
	channelB := parseChannel("GOCAN_PCAN_CHANNEL_B", channelBValue)
	fdBitrate := os.Getenv("GOCAN_PCAN_FD_BITRATE")

	open := func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		configA := Config{ID: 1, Name: "pcan-a", Channel: channelA}
		configB := Config{ID: 2, Name: "pcan-b", Channel: channelB}
		if fdBitrate == "" {
			configA.Bitrate = Bitrate500K
			configB.Bitrate = Bitrate500K
		} else {
			configA.FDBitrate = fdBitrate
			configB.FDBitrate = fdBitrate
		}

		a, err := Open(context.Background(), capture, configA)
		if err != nil {
			t.Fatalf("Open a: %v", err)
		}
		t.Cleanup(func() { _ = a.Close() })

		b, err := Open(context.Background(), capture, configB)
		if err != nil {
			_ = a.Close()
			t.Fatalf("Open b: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return a, b
	}

	conformance.Run(t, open, conformance.Capabilities{
		FD:           fdBitrate != "",
		RemoteFrames: true,
	})
}
