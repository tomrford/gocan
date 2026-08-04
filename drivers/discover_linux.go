//go:build linux

package drivers

import "github.com/tomrford/gocan/drivers/socketcan"

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
			Driver:             "socketcan",
			Name:               network.Name,
			SupportsFD:         network.SupportsFD,
			SocketCANInterface: network.Name,
		}
	}
	return channels, nil
}
