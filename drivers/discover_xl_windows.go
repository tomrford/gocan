//go:build amd64

package drivers

import "github.com/tomrford/gocan/drivers/vector"

func discoverVector() ([]Channel, error) {
	vectorChannels, err := vector.Discover()
	channels := make([]Channel, 0, len(vectorChannels))
	for _, channel := range vectorChannels {
		channels = append(channels, Channel{
			Driver:             "vector",
			Name:               channel.Name,
			SupportsFD:         channel.SupportsFD,
			VectorChannelIndex: channel.ChannelIndex,
		})
	}
	return channels, err
}
