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
	mode, err := validateOpen(capture, channel, config)
	if err != nil {
		return nil, err
	}
	switch channel.driver {
	case driverPCAN:
		return openPCAN(ctx, capture, channel, config, mode)
	case driverVector:
		return openVector(ctx, capture, channel, config, mode)
	default:
		return nil, fmt.Errorf("driver %q is not available on Windows", channel.Driver())
	}
}

func openPCAN(ctx context.Context, capture *gocan.Capture, channel Channel, config Config, mode configMode) (gocan.Bus, error) {
	native, err := nativePCANConfig(channel, config, mode)
	if err != nil {
		return nil, err
	}
	return pcan.Open(ctx, capture, native)
}

func nativePCANConfig(channel Channel, config Config, mode configMode) (pcan.Config, error) {
	native := pcan.Config{
		ID:      config.ID,
		Name:    config.Name,
		Channel: pcan.Channel(channel.native),
	}
	switch mode {
	case modeClassic:
		if config.Bitrate != 500_000 {
			return pcan.Config{}, fmt.Errorf("PCAN classical bitrate %d is unsupported", config.Bitrate)
		}
		native.Bitrate = pcan.Bitrate500K
	case modeFD:
		native.FDBitrate = pcanFDBitrate(config.FDTiming)
	default:
		return pcan.Config{}, invalidModeError(mode)
	}
	return native, nil
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

func invalidModeError(mode configMode) error {
	return fmt.Errorf("unsupported physical CAN configuration mode %d", mode)
}
