// Package drivers discovers and opens physical CAN channels. Native driver
// selectors and configuration remain private to this package.
package drivers

import "fmt"

type driverKind uint8

const (
	driverUnknown driverKind = iota
	driverSocketCAN
	driverPCAN
	driverVector
	driverNIXNET
)

func (driver driverKind) String() string {
	switch driver {
	case driverSocketCAN:
		return "socketcan"
	case driverPCAN:
		return "pcan"
	case driverNIXNET:
		return "nixnet"
	case driverVector:
		return "vector"
	default:
		return ""
	}
}

// Channel is one physical CAN channel returned by Discover. Its native
// selector is intentionally opaque; pass the value unchanged to Open.
type Channel struct {
	driver              driverKind
	name                string
	nativeName          string
	native              uint64
	supportsFD          bool
	external            bool
	externalBitrate     uint32
	externalDataBitrate uint32
	supportsTermination bool
}

// Driver returns "socketcan", "pcan", "vector", or "nixnet".
func (channel Channel) Driver() string { return channel.driver.String() }

// Name returns the device or interface name reported by the native stack.
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
	switch channel.driver {
	case driverSocketCAN:
		return "socketcan:" + channel.nativeName
	case driverPCAN:
		return fmt.Sprintf("pcan:%#x", channel.native)
	case driverNIXNET:
		return "nixnet:" + channel.nativeName
	case driverVector:
		return fmt.Sprintf("vector:%d", channel.native)
	default:
		return ""
	}
}

// SupportsTermination reports whether gocan can switch the channel's bus termination.
func (channel Channel) SupportsTermination() bool { return channel.supportsTermination }

// String returns the channel's driver, native name, and identifier, for example
// "pcan PCAN_USBBUS1 (pcan:0x51)".
func (channel Channel) String() string {
	return fmt.Sprintf("%s %s (%s)", channel.Driver(), channel.Name(), channel.Identifier())
}
