// Package drivers discovers and opens physical CAN channels. Native driver
// selectors and configuration remain private to this package.
package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
)

type preparedOpen func(context.Context, *gocan.Capture) (gocan.Bus, error)

type driverDescriptor struct {
	name             string
	formatIdentifier func(Channel) string
	fdRatePresets    func() []FDRatePreset
	prepare          func(Channel, preparedConfig) (preparedOpen, error)
	discover         func() ([]Channel, error)
}

func (driver *driverDescriptor) discoverChannels() ([]Channel, error) {
	channels, err := driver.discover()
	for i := range channels {
		channels[i].driver = driver
	}
	return channels, err
}

// Channel is one physical CAN channel returned by Discover. Its native
// selector is intentionally opaque; pass the value unchanged to Open.
type Channel struct {
	driver              *driverDescriptor
	name                string
	nativeName          string
	native              uint64
	supportsFD          bool
	external            bool
	externalBitrate     uint32
	externalDataBitrate uint32
	supportsTermination bool
}

// Driver returns "socketcan", "pcan", "vector", "nixnet", or "zlg".
func (channel Channel) Driver() string {
	if channel.driver == nil {
		return ""
	}
	return channel.driver.name
}

// Name returns the device or interface name reported by the native stack.
// PCAN and ZLG report one name per device, so their names append a number
// unique within the driver: the PCAN bus number, as in "PCAN-USB (1)" for
// PCAN_USBBUS1, or the ZLG device index and channel, as in
// "USBCANFD-200U (0:1)".
func (channel Channel) Name() string { return channel.name }

// SupportsFD reports whether the channel supports CAN FD.
func (channel Channel) SupportsFD() bool { return channel.supportsFD }

// ExternallyConfigured reports whether something other than gocan owns the
// channel's bit timing: Linux for SocketCAN, or, for Vector, another port
// that had the channel on the bus when Discover ran. That includes ports this
// process opened.
func (channel Channel) ExternallyConfigured() bool { return channel.external }

// ExternalBitrates reports the nominal and CAN FD data rates in bits per
// second of an externally configured Vector channel. Zero means unknown; a
// zero data rate also means classical CAN or an interface that does not report
// its CAN FD mode.
func (channel Channel) ExternalBitrates() (bitrate, dataBitrate uint32) {
	return channel.externalBitrate, channel.externalDataBitrate
}

// Identifier returns a session-local display key which distinguishes channels
// that have the same driver and name. Pass Channel itself to Open.
func (channel Channel) Identifier() string {
	if channel.driver == nil {
		return ""
	}
	return channel.driver.formatIdentifier(channel)
}

// SupportsTermination reports whether gocan can switch the channel's bus termination.
func (channel Channel) SupportsTermination() bool { return channel.supportsTermination }

// String returns the channel's driver, name, and identifier, for example
// "pcan PCAN-USB (1) (pcan:0x51)".
func (channel Channel) String() string {
	return fmt.Sprintf("%s %s (%s)", channel.Driver(), channel.Name(), channel.Identifier())
}

// Discover reports physical CAN channels available on this host.
//
// Platform behavior:
//   - Linux: reports SocketCAN interfaces in kernel order, including interfaces
//     that are down.
//   - Windows: reports attached PCAN channels followed by CAN-capable Vector,
//     NI-XNET, and ZLG channels, each in native driver order. Vector, NI-XNET,
//     and ZLG are discovered on windows/amd64. A vendor stack that is not
//     installed contributes neither channels nor an error; an installed stack
//     that fails to answer contributes no channels and joins its error into the
//     returned error alongside the other drivers' channels.
//   - Other platforms: reports no channels and a nil error.
func Discover() ([]Channel, error) {
	return discoverChannels()
}
