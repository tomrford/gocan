//go:build windows

package pcan

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

func TestPCANOpenRejectsUnknownClassicalBitrate(t *testing.T) {
	_, err := Open(context.Background(), gocan.NewCapture(), Config{
		ID: 1, Name: "can", Channel: ChannelUSB1, Bitrate: 300_000,
	})
	if err == nil || !strings.Contains(err.Error(), "predefined") {
		t.Fatalf("Open with an unknown bitrate = %v", err)
	}
}

// TestPCANClassicalBitrateMatrix opens every configured adapter at each
// predefined classical bitrate and has each one send a frame that every
// other adapter must receive intact.
func TestPCANClassicalBitrateMatrix(t *testing.T) {
	channels := []Channel{
		testChannel(t, "GOCAN_PCAN_CHANNEL_A"),
		testChannel(t, "GOCAN_PCAN_CHANNEL_B"),
	}
	if os.Getenv("GOCAN_PCAN_CHANNEL_C") != "" {
		channels = append(channels, testChannel(t, "GOCAN_PCAN_CHANNEL_C"))
	}

	for _, rate := range slices.Sorted(maps.Keys(classicalBitrates)) {
		t.Run(fmt.Sprintf("%dbps", rate), func(t *testing.T) {
			capture := gocan.NewCapture()
			buses := make([]gocan.Bus, len(channels))
			for index, channel := range channels {
				bus, err := Open(context.Background(), capture, Config{
					ID:      gocan.BusID(index + 1),
					Name:    fmt.Sprintf("pcan-rate-%d", index+1),
					Channel: channel,
					Bitrate: rate,
				})
				if err != nil {
					t.Fatalf("Open channel %#x at %d bit/s: %v", channel, rate, err)
				}
				t.Cleanup(func() { _ = bus.Close() })
				buses[index] = bus
			}

			for senderIndex, sender := range buses {
				payload := []byte{byte(senderIndex), byte(rate >> 16), byte(rate >> 8), byte(rate)}
				frame, err := gocan.NewFrame(0x500+uint32(senderIndex), payload, 0)
				if err != nil {
					t.Fatalf("NewFrame: %v", err)
				}
				cursor := capture.End()
				if err := sender.Send(context.Background(), frame); err != nil {
					t.Fatalf("send from bus %d at %d bit/s: %v", sender.ID(), rate, err)
				}
				for _, receiver := range buses {
					if receiver == sender {
						continue
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					received, _, err := capture.Next(ctx, gocan.FrameKey{
						Bus:       receiver.ID(),
						ID:        frame.ID,
						Direction: gocan.DirectionReceive,
					}, cursor)
					cancel()
					if err != nil {
						t.Fatalf("receive on bus %d at %d bit/s: %v", receiver.ID(), rate, err)
					}
					if received.Frame != frame {
						t.Fatalf("bus %d at %d bit/s received %+v, want %+v", receiver.ID(), rate, received.Frame, frame)
					}
				}
			}
		})
	}
}
