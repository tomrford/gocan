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
		channels = append(channels, Channel{
			name:                c.Name,
			nativeName:          c.Interface,
			supportsFD:          c.SupportsFD,
			supportsTermination: c.SupportsTermination,
		})
	}
	return channels, err
}

func openNIXNET(ctx context.Context, capture *gocan.Capture, config nixnet.Config) (gocan.Bus, error) {
	bus, err := nixnet.Open(ctx, capture, config)
	if err != nil {
		return nil, err
	}
	return bus, nil
}
