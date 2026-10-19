//go:build linux

package drivers

import (
	"context"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/socketcan"
)

// Open opens a discovered SocketCAN interface. Linux owns its bit timing;
// Config.External must be true. Context controls opening only.
//
// Classical DLC values 9-15 survive only on interfaces with cc-len8-dlc
// enabled (or on vcan); other interfaces send and report them as DLC 8.
func Open(ctx context.Context, capture *gocan.Capture, channel Channel, config Config) (gocan.Bus, error) {
	if _, err := prepareOpen(capture, channel, config); err != nil {
		return nil, err
	}
	return socketcan.Open(ctx, capture, socketcan.Config{
		ID: config.ID, Name: config.Name, Interface: channel.nativeName, FD: channel.supportsFD,
	})
}
