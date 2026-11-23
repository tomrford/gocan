package drivers

import (
	"context"

	"github.com/tomrford/gocan"
)

// Open configures and opens a discovered physical CAN channel. Open validates
// channel and config before consulting ctx; invalid input fails with an error
// matching ErrInvalidConfig. Context controls opening only; canceling it after
// Open returns does not stop the bus.
//
// Platform behavior:
//   - Linux: opens a discovered SocketCAN interface. Linux owns bit timing, so
//     Config.External must be true.
//   - Windows: opens a discovered PCAN, Vector, NI-XNET, or ZLG channel.
//     Vector and NI-XNET are supported on windows/amd64.
//   - Other platforms: returns ErrInvalidConfig because Discover returns no
//     channels.
func Open(ctx context.Context, capture *gocan.Capture, channel Channel, config Config) (gocan.Bus, error) {
	open, err := prepareOpen(capture, channel, config)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return open(ctx, capture)
}
