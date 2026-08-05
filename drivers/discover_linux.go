//go:build linux

package drivers

import "github.com/tomrford/gocan/drivers/internal/socketcan"

// Discover reports the SocketCAN interfaces on this host, including
// interfaces which are down, in kernel order.
func Discover() ([]Channel, error) {
	interfaces, err := socketcan.Discover()
	if err != nil {
		return nil, err
	}
	channels := make([]Channel, len(interfaces))
	for index, network := range interfaces {
		channels[index] = Channel{
			driver:     driverSocketCAN,
			name:       network.Name,
			nativeName: network.Name,
			supportsFD: network.SupportsFD,
			external:   true,
		}
	}
	return channels, nil
}
