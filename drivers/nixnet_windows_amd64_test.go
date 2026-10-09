//go:build windows && amd64

package drivers

import (
	"context"
	"fmt"
	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
	"os"
	"testing"
	"time"
)

func nixnetPair(t testing.TB, capture *gocan.Capture, dataRate uint32, concurrent bool) (gocan.Bus, gocan.Bus) {
	t.Helper()
	a, b := os.Getenv("GOCAN_NIXNET_CHANNEL_A"), os.Getenv("GOCAN_NIXNET_CHANNEL_B")
	if a == "" || b == "" {
		t.Skip("GOCAN_NIXNET_CHANNEL_A/B not set")
	}
	if a == b {
		t.Fatal("NI-XNET pair requires distinct connected channels")
	}
	channels, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	pair := make([]gocan.Bus, 0, 2)
	for index, name := range []string{a, b} {
		found := false
		for _, channel := range channels {
			if channel.Identifier() != "nixnet:"+name {
				continue
			}
			found = true
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			bus, err := Open(ctx, capture, channel, Config{ID: gocan.BusID(index + 1), Name: name, Bitrate: 500000, DataBitrate: dataRate, Termination: TerminationOn, NIXNETConcurrentIO: concurrent})
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := bus.Close(); err != nil {
					t.Error(err)
				}
			})
			pair = append(pair, bus)
			break
		}
		if !found {
			t.Fatalf("NI-XNET %s not discovered", name)
		}
	}
	return pair[0], pair[1]
}

func TestNIXNETConformanceHardware(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent_%t", concurrent), func(t *testing.T) {
			for _, rate := range []uint32{0, 2000000, 4000000} {
				t.Run(fmt.Sprintf("data_%d", rate), func(t *testing.T) {
					conformance.Run(t, func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
						return nixnetPair(t, capture, rate, concurrent)
					}, conformance.Capabilities{FD: rate != 0, RemoteFrames: true})
				})
			}
		})
	}
}

func TestNIXNETFDLengthsHardware(t *testing.T) {
	for _, rate := range []uint32{2000000, 4000000} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			capture := gocan.NewCapture()
			a, b := nixnetPair(t, capture, rate, false)
			for _, flags := range []gocan.FrameFlags{gocan.FrameFD, gocan.FrameFD | gocan.FrameBitRateSwitch, gocan.FrameFD | gocan.FrameExtended | gocan.FrameBitRateSwitch} {
				for dlc := uint8(0); dlc <= 15; dlc++ {
					frame := gocan.Frame{ID: 0x500 + uint32(dlc), DLC: dlc, Flags: flags}
					for i := 0; i < frame.DataLength(); i++ {
						frame.Data[i] = byte(i + int(dlc))
					}
					for _, side := range [][2]gocan.Bus{{a, b}, {b, a}} {
						cursor := capture.End()
						if err := side[0].Send(context.Background(), frame); err != nil {
							t.Fatal(err)
						}
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						event, _, err := capture.Next(ctx, gocan.FrameKey{Bus: side[1].ID(), ID: frame.ID, Extended: flags.Has(gocan.FrameExtended), Direction: gocan.DirectionReceive}, cursor)
						cancel()
						if err != nil || event.Frame != frame {
							t.Fatalf("FD DLC%d flags%x: got %+v %v", dlc, flags, event.Frame, err)
						}
					}
				}
			}

		})
	}
}

func BenchmarkNIXNETHardware(b *testing.B) {
	for _, rate := range []uint32{0, 2000000} {
		b.Run(fmt.Sprintf("data_%d", rate), func(b *testing.B) {
			capture := gocan.NewCapture()
			a, peer := nixnetPair(b, capture, rate, false)
			flags := gocan.FrameFlags(0)
			length := 8
			if rate != 0 {
				flags = gocan.FrameFD | gocan.FrameBitRateSwitch
				length = 64
			}
			frame, err := gocan.NewFrame(0x123, make([]byte, length), flags)
			if err != nil {
				b.Fatal(err)
			}
			conformance.RoundTripBenchmark(b, capture, a, peer, frame)
		})
	}
}

func BenchmarkNIXNETSaturatedHardware(b *testing.B) {
	for _, rate := range []uint32{0, 2000000} {
		b.Run(fmt.Sprintf("data_%d", rate), func(b *testing.B) {
			capture := gocan.NewCapture()
			a, peer := nixnetPair(b, capture, rate, false)
			flags := gocan.FrameFlags(0)
			length := 8
			if rate != 0 {
				flags = gocan.FrameFD | gocan.FrameBitRateSwitch
				length = 64
			}
			frame, err := gocan.NewFrame(0x123, make([]byte, length), flags)
			if err != nil {
				b.Fatal(err)
			}
			conformance.SaturatedCaptureBenchmark(b, capture, a, peer, frame)
		})
	}
}
