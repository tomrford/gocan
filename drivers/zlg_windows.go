//go:build windows

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/zlg"
)

func discoverZLG() ([]Channel, error) {
	zlgChannels, err := zlg.Discover()
	channels := make([]Channel, 0, len(zlgChannels))
	for _, channel := range zlgChannels {
		channels = append(channels, zlgChannel(
			fmt.Sprintf("%s (%d:%d)", channel.HardwareType, channel.Device, channel.Channel),
			channel.DeviceType,
			channel.Device,
			channel.Channel,
			channel.SupportsFD,
		))
	}
	return channels, err
}

func openZLG(ctx context.Context, capture *gocan.Capture, config zlg.Config) (gocan.Bus, error) {
	return zlg.Open(ctx, capture, config)
}
