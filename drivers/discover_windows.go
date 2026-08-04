//go:build windows

package drivers

import (
	"errors"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/pcan"
)

// Discover reports the attached PCAN channels followed by the CAN-capable
// Vector channels, each in native driver order. A vendor stack that is not
// installed contributes neither channels nor an error; an installed stack
// that fails to answer contributes no channels and joins its error into the
// returned error alongside the other driver's channels.
func Discover() ([]Channel, error) {
	pcanChannels, pcanErr := pcan.Discover()
	vectorChannels, vectorErr := discoverVector()
	if errors.Is(pcanErr, gocan.ErrDriverUnavailable) {
		pcanErr = nil
	}
	if errors.Is(vectorErr, gocan.ErrDriverUnavailable) {
		vectorErr = nil
	}

	channels := make([]Channel, 0, len(pcanChannels)+len(vectorChannels))
	for _, channel := range pcanChannels {
		channels = append(channels, Channel{
			Driver:      "pcan",
			Name:        channel.Name,
			SupportsFD:  channel.SupportsFD,
			PCANChannel: channel.Channel,
		})
	}
	channels = append(channels, vectorChannels...)
	return channels, errors.Join(pcanErr, vectorErr)
}
