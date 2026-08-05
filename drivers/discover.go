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
)

func (driver driverKind) String() string {
	switch driver {
	case driverSocketCAN:
		return "socketcan"
	case driverPCAN:
		return "pcan"
	case driverVector:
		return "vector"
	default:
		return ""
	}
}

// Channel is one physical CAN channel returned by Discover. Its native
// selector is intentionally opaque; pass the value unchanged to Open.
type Channel struct {
	driver     driverKind
	name       string
	nativeName string
	native     uint64
	supportsFD bool
	external   bool
}

// Driver returns "socketcan", "pcan", or "vector".
func (channel Channel) Driver() string { return channel.driver.String() }

// Name returns the device or interface name reported by the native stack.
func (channel Channel) Name() string { return channel.name }

// SupportsFD reports whether the channel supports CAN FD.
func (channel Channel) SupportsFD() bool { return channel.supportsFD }

// ExternallyConfigured reports whether the operating system, rather than
// gocan, owns the channel's bit timing.
func (channel Channel) ExternallyConfigured() bool { return channel.external }

// Identifier returns a session-local display key which distinguishes channels
// that have the same driver and name. Pass Channel itself to Open.
func (channel Channel) Identifier() string {
	switch channel.driver {
	case driverSocketCAN:
		return "socketcan:" + channel.nativeName
	case driverPCAN:
		return fmt.Sprintf("pcan:%#x", channel.native)
	case driverVector:
		return fmt.Sprintf("vector:%d", channel.native)
	default:
		return ""
	}
}
