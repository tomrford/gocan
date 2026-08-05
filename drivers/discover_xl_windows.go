//go:build amd64

package drivers

import "github.com/tomrford/gocan/drivers/internal/vector"

func discoverVector() ([]Channel, error) {
	vectorChannels, err := vector.Discover()
	channels := make([]Channel, 0, len(vectorChannels))
	for _, channel := range vectorChannels {
		channels = append(channels, Channel{
			driver:     driverVector,
			name:       channel.Name,
			native:     uint64(channel.ChannelIndex),
			supportsFD: channel.SupportsFD,
		})
	}
	return channels, err
}
