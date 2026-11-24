package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/pcan"
)

var pcanDriver = driverDescriptor{
	name: "pcan",
	formatIdentifier: func(channel Channel) string {
		return fmt.Sprintf("pcan:%#x", channel.native)
	},
	fdRatePresets: func() []FDRatePreset {
		return presetRates(fdPresets80MHz[:])
	},
	prepare: func(channel Channel, prepared preparedConfig) (preparedOpen, error) {
		native, err := nativePCANConfig(channel, prepared)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, capture *gocan.Capture) (gocan.Bus, error) {
			return openPCAN(ctx, capture, native)
		}, nil
	},
	discover: discoverPCAN,
}

func nativePCANConfig(channel Channel, prepared preparedConfig) (pcan.Config, error) {
	if prepared.config.Vendor != nil {
		return pcan.Config{}, rejectVendorOptions(channel, prepared.config.Vendor)
	}
	native := pcan.Config{
		ID:      prepared.config.ID,
		Name:    prepared.config.Name,
		Channel: pcan.Channel(channel.native),
	}
	if prepared.fd {
		timing, _, _, err := resolveFDTiming(prepared.config, fdPresets80MHz[:])
		if err != nil {
			return pcan.Config{}, err
		}
		if err := validatePCANFDTiming(timing); err != nil {
			return pcan.Config{}, err
		}
		native.FDBitrate = pcanFDBitrate(timing)
		return native, nil
	}
	if !pcan.ValidClassicalBitrate(prepared.config.Bitrate) {
		return pcan.Config{}, fmt.Errorf("%w: PCAN classical bitrate %d is not a PCAN-Basic predefined rate", ErrInvalidConfig, prepared.config.Bitrate)
	}
	native.Bitrate = prepared.config.Bitrate
	return native, nil
}

func validatePCANFDTiming(timing FDTiming) error {
	switch timing.ClockHz {
	case 20_000_000, 24_000_000, 30_000_000, 40_000_000, 60_000_000, 80_000_000:
	default:
		return fmt.Errorf("%w: PCAN CAN FD clock %d Hz is not supported (must be 20, 24, 30, 40, 60, or 80 MHz)", ErrInvalidConfig, timing.ClockHz)
	}
	if timing.Nominal.BRP > 1024 || timing.Nominal.TSEG1 > 256 || timing.Nominal.TSEG2 > 128 || timing.Nominal.SJW > 128 {
		return fmt.Errorf("%w: PCAN CAN FD timing exceeds nominal phase limits", ErrInvalidConfig)
	}
	if timing.Data.BRP > 1024 || timing.Data.TSEG1 > 32 || timing.Data.TSEG2 > 16 || timing.Data.SJW > 16 {
		return fmt.Errorf("%w: PCAN CAN FD timing exceeds data phase limits", ErrInvalidConfig)
	}
	return nil
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
