//go:build windows && amd64

package drivers

import (
	"context"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/vector"
)

func discoverVector() ([]Channel, error) {
	vectorChannels, err := vector.Discover()
	channels := make([]Channel, 0, len(vectorChannels))
	for _, channel := range vectorChannels {
		channels = append(channels, Channel{
			name:                channel.Name,
			native:              uint64(channel.ChannelIndex),
			supportsFD:          channel.SupportsFD,
			external:            channel.OnBus,
			externalBitrate:     channel.Bitrate,
			externalDataBitrate: channel.DataBitrate,
		})
	}
	return channels, err
}

func openVector(ctx context.Context, capture *gocan.Capture, config vector.Config) (gocan.Bus, error) {
	return vector.Open(ctx, capture, config)
}
