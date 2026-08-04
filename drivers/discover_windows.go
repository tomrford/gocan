//go:build windows

package drivers

import (
	"errors"

	"github.com/tomrford/gocan/drivers/pcan"
	"github.com/tomrford/gocan/drivers/vector"
)

// Discover reports the attached PCAN channels followed by the CAN-capable
// Vector channels, each in native driver order. A driver whose stack fails
// to answer contributes no channels and joins its error into the returned
// error, so one missing vendor stack does not hide the other's channels.
func Discover() ([]Channel, error) {
	pcanChannels, pcanErr := pcan.Discover()
	vectorChannels, vectorErr := vector.Discover()

	channels := make([]Channel, 0, len(pcanChannels)+len(vectorChannels))
	for _, channel := range pcanChannels {
		channels = append(channels, Channel{
			Driver:      "pcan",
			Name:        channel.Name,
			SupportsFD:  channel.SupportsFD,
			PCANChannel: channel.Channel,
		})
	}
	for _, channel := range vectorChannels {
		channels = append(channels, Channel{
			Driver:             "vector",
			Name:               channel.Name,
			SupportsFD:         channel.SupportsFD,
			VectorChannelIndex: channel.ChannelIndex,
		})
	}
	return channels, errors.Join(pcanErr, vectorErr)
}
