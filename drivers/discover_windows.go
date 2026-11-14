//go:build windows

package drivers

import (
	"errors"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/pcan"
	"github.com/tomrford/gocan/drivers/internal/zlg"
)

// Discover reports the attached PCAN channels followed by the CAN-capable
// Vector, NI-XNET, and ZLG channels, each in native driver order. ZLG adds
// about one second per attached adapter, which it opens and closes to read its
// channel count; adapters held by another process are omitted. A vendor stack that is not
// installed contributes neither channels nor an error; an installed stack
// that fails to answer contributes no channels and joins its error into the
// returned error alongside the other driver's channels.
func Discover() ([]Channel, error) {
	pcanChannels, pcanErr := pcan.Discover()
	vectorChannels, vectorErr := discoverVector()
	nixnetChannels, nixnetErr := discoverNIXNET()
	zlgChannels, zlgErr := zlg.Discover()
	if errors.Is(nixnetErr, gocan.ErrDriverUnavailable) {
		nixnetErr = nil
	}
	if errors.Is(pcanErr, gocan.ErrDriverUnavailable) {
		pcanErr = nil
	}
	if errors.Is(vectorErr, gocan.ErrDriverUnavailable) {
		vectorErr = nil
	}
	if errors.Is(zlgErr, gocan.ErrDriverUnavailable) {
		zlgErr = nil
	}

	channels := make([]Channel, 0, len(pcanChannels)+len(vectorChannels)+len(nixnetChannels)+len(zlgChannels))
	for _, channel := range pcanChannels {
		channels = append(channels, Channel{
			driver:     driverPCAN,
			name:       channel.Name,
			native:     uint64(channel.Channel),
			supportsFD: channel.SupportsFD,
		})
	}
	channels = append(channels, vectorChannels...)
	channels = append(channels, nixnetChannels...)
	for _, channel := range zlgChannels {
		channels = append(channels, Channel{
			driver:     driverZLG,
			name:       channel.HardwareType,
			nativeName: channel.Serial,
			native:     uint64(channel.Device)<<32 | uint64(channel.Channel),
		})
	}
	return channels, errors.Join(pcanErr, vectorErr, nixnetErr, zlgErr)
}
