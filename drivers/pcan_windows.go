//go:build windows

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/pcan"
)

func discoverPCAN() ([]Channel, error) {
	pcanChannels, err := pcan.Discover()
	channels := make([]Channel, 0, len(pcanChannels))
	for _, channel := range pcanChannels {
		channels = append(channels, Channel{
			name:       fmt.Sprintf("%s (%d)", channel.Name, channel.Channel.Bus()),
			native:     uint64(channel.Channel),
			supportsFD: channel.SupportsFD,
		})
	}
	return channels, err
}

func openPCAN(ctx context.Context, capture *gocan.Capture, config pcan.Config) (gocan.Bus, error) {
	bus, err := pcan.Open(ctx, capture, config)
	if err != nil {
		return nil, err
	}
	return bus, nil
}
