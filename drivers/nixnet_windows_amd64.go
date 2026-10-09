//go:build windows && amd64

package drivers

import (
	"context"
	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/nixnet"
)

func discoverNIXNET() ([]Channel, error) {
	native, err := nixnet.Discover()
	channels := make([]Channel, 0, len(native))
	for _, c := range native {
		channels = append(channels, Channel{driver: driverNIXNET, name: c.Name, nativeName: c.Interface, supportsFD: c.SupportsFD, supportsTermination: c.SupportsTermination})
	}
	return channels, err
}
func openNIXNET(ctx context.Context, capture *gocan.Capture, channel Channel, config Config, fd bool) (gocan.Bus, error) {
	native := nixnet.Config{ID: config.ID, Name: config.Name, Interface: channel.nativeName, Baud: uint64(config.Bitrate), FD: fd, Termination: uint8(config.Termination), ConcurrentIO: config.NIXNETConcurrentIO}
	if fd {
		nominal, data, err := nixnetFDTiming(config.FDTiming)
		if err != nil {
			return nil, err
		}
		native.Baud = nominal
		native.FDBaud = data
	}
	return nixnet.Open(ctx, capture, native)
}
