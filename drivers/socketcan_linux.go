//go:build linux

package drivers

import (
	"context"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/socketcan"
)

func discoverSocketCAN() ([]Channel, error) {
	interfaces, err := socketcan.Discover()
	if err != nil {
		return nil, err
	}
	channels := make([]Channel, len(interfaces))
	for index, network := range interfaces {
		channels[index] = Channel{
			name:       network.Name,
			nativeName: network.Name,
			supportsFD: network.SupportsFD,
			external:   true,
		}
	}
	return channels, nil
}

func openSocketCAN(ctx context.Context, capture *gocan.Capture, cfg socketcanPrepared) (gocan.Bus, error) {
	return socketcan.Open(ctx, capture, socketcan.Config{
		ID:        cfg.id,
		Name:      cfg.name,
		Interface: cfg.iface,
		FD:        cfg.fd,
	})
}
