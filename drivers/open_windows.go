//go:build windows

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/pcan"
)

// Open configures and opens a discovered physical CAN channel. Context
// controls opening only; canceling it after Open returns does not stop the bus.
func Open(ctx context.Context, capture *gocan.Capture, channel Channel, config Config) (gocan.Bus, error) {
	fd, err := validateOpen(capture, channel, config)
	if err != nil {
		return nil, err
	}
	switch channel.driver {
	case driverPCAN:
		return pcan.Open(ctx, capture, nativePCANConfig(channel, config, fd))
	case driverVector:
		return openVector(ctx, capture, channel, config, fd)
	default:
		return nil, fmt.Errorf("driver %q is not available on Windows", channel.Driver())
	}
}

func nativePCANConfig(channel Channel, config Config, fd bool) pcan.Config {
	native := pcan.Config{
		ID:      config.ID,
		Name:    config.Name,
		Channel: pcan.Channel(channel.native),
	}
	if fd {
		native.FDBitrate = pcanFDBitrate(config.FDTiming)
	} else {
		native.Bitrate = config.Bitrate
	}
	return native
}

func pcanFDBitrate(timing FDTiming) string {
	return fmt.Sprintf(
		"f_clock=%d, nom_brp=%d, nom_tseg1=%d, nom_tseg2=%d, nom_sjw=%d, data_brp=%d, data_tseg1=%d, data_tseg2=%d, data_sjw=%d",
		timing.ClockHz,
		timing.Nominal.BRP,
		timing.Nominal.TSEG1,
		timing.Nominal.TSEG2,
		timing.Nominal.SJW,
		timing.Data.BRP,
		timing.Data.TSEG1,
		timing.Data.TSEG2,
		timing.Data.SJW,
	)
}
