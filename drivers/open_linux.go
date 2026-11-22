//go:build linux

package drivers

import (
	"context"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/socketcan"
)

func openChannel(ctx context.Context, capture *gocan.Capture, channel Channel, config Config) (gocan.Bus, error) {
	if _, err := prepareOpen(capture, channel, config); err != nil {
		return nil, err
	}
	return socketcan.Open(ctx, capture, socketcan.Config{
		ID: config.ID, Name: config.Name, Interface: channel.nativeName, FD: channel.supportsFD,
	})
}
