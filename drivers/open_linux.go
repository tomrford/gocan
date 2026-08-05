//go:build linux

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/socketcan"
)

// Open opens a discovered SocketCAN interface. Linux owns its bit timing;
// Config.External must be true. Context controls opening only.
func Open(ctx context.Context, capture *gocan.Capture, channel Channel, config Config) (gocan.Bus, error) {
	if _, err := validateOpen(capture, channel, config); err != nil {
		return nil, err
	}
	if channel.driver != driverSocketCAN {
		return nil, fmt.Errorf("driver %q is not available on Linux", channel.Driver())
	}
	return socketcan.Open(ctx, capture, socketcan.Config{
		ID: config.ID, Name: config.Name, Interface: channel.nativeName, FD: channel.supportsFD,
	})
}
