// Package drivers aggregates the physical CAN drivers behind one
// platform-wide discovery call. Open a discovered channel with the owning
// driver package; the virtual driver creates buses on demand and reports no
// channels here.
package drivers

import (
	"github.com/tomrford/gocan/drivers/pcan"
	"github.com/tomrford/gocan/drivers/vector"
)

// Channel is one CAN channel reported by a physical driver on this platform.
// It carries the shape every driver shares; detail beyond it comes from the
// owning driver's own Discover.
type Channel struct {
	// Driver is the reporting driver package: "socketcan", "pcan", or
	// "vector".
	Driver string
	// Name is the device or interface name reported by the native stack.
	Name string
	// SupportsFD reports whether the owning driver supports CAN FD on this
	// channel.
	SupportsFD bool

	// Exactly one selector field, matching Driver, identifies the channel to
	// that driver's Config.

	// SocketCANInterface is the Linux network interface name.
	SocketCANInterface string
	// PCANChannel is the PCAN-Basic channel handle.
	PCANChannel pcan.Channel
	// VectorChannelIndex is the XL global channel index.
	VectorChannelIndex vector.ChannelIndex
}
