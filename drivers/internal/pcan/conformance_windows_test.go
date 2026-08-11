//go:build windows

package pcan

import (
	"context"
	"os"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
)

func TestPCANConformance(t *testing.T) {
	channelA := testChannel(t, "GOCAN_PCAN_CHANNEL_A")
	channelB := testChannel(t, "GOCAN_PCAN_CHANNEL_B")
	fdBitrate := os.Getenv("GOCAN_PCAN_FD_BITRATE")

	open := func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		configA := Config{ID: 1, Name: "pcan-a", Channel: channelA}
		configB := Config{ID: 2, Name: "pcan-b", Channel: channelB}
		if fdBitrate == "" {
			configA.Bitrate = 500_000
			configB.Bitrate = 500_000
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
