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
//     Config.External must be true. Classical DLC values 9-15 survive only on
//     interfaces with cc-len8-dlc enabled (or on vcan); other interfaces send
//     and report them as DLC 8.
//   - Windows: opens a discovered PCAN, Vector, NI-XNET, or ZLG channel.
//     NI-XNET uses ISO FD, rejects classical DLC above 8 and transmit ESI, and
//     cannot report receive ESI; its FD timing uses multiples of 25 ns, with
//     transmitter delay compensation derived from the sample point above
//     1 Mbit/s. Vector and NI-XNET are supported on windows/amd64.
//   - Other platforms: returns an error because no physical CAN driver is
//     available.
func Open(ctx context.Context, capture *gocan.Capture, channel Channel, config Config) (gocan.Bus, error) {
	return openChannel(ctx, capture, channel, config)
}
