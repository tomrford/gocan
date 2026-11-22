package drivers

import (
	"context"

	"github.com/tomrford/gocan"
)

// Open configures and opens a discovered physical CAN channel. Context controls
// opening only; canceling it after Open returns does not stop the bus.
//
// Platform behavior:
//   - Linux: opens a discovered SocketCAN interface. Linux owns bit timing, so
//     Config.External must be true.
//   - Windows: opens a discovered PCAN, Vector, NI-XNET, or ZLG channel.
//     Vector and NI-XNET are supported on windows/amd64.
//   - Other platforms: returns an error because no physical CAN driver is
//     available.
func Open(ctx context.Context, capture *gocan.Capture, channel Channel, config Config) (gocan.Bus, error) {
	return openChannel(ctx, capture, channel, config)
}
