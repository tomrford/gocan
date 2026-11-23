package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/vector"
)

const vectorFDClockHz = 80_000_000

var vectorDriver = driverDescriptor{
	name: "vector",
	formatIdentifier: func(channel Channel) string {
		return fmt.Sprintf("vector:%d", channel.native)
	},
	fdRatePresets: func() []FDRatePreset {
		return presetRates(fdPresets80MHz[:])
	},
	prepare: func(channel Channel, prepared preparedConfig) (preparedOpen, error) {
		native, err := nativeVectorConfig(channel, prepared)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, capture *gocan.Capture) (gocan.Bus, error) {
			return openVector(ctx, capture, native)
		}, nil
	},
	discover: discoverVector,
}

func nativeVectorConfig(channel Channel, prepared preparedConfig) (vector.Config, error) {
	if prepared.config.Vendor != nil {
		return vector.Config{}, rejectVendorOptions(channel, prepared.config.Vendor)
	}
	if channel.native >= 64 {
		return vector.Config{}, fmt.Errorf("%w: Vector channel index %d exceeds 63", ErrInvalidConfig, channel.native)
	}
	native := vector.Config{
		ID:           prepared.config.ID,
		Name:         prepared.config.Name,
		ChannelIndex: vector.ChannelIndex(channel.native),
		Join:         prepared.config.External,
	}
	if prepared.config.External {
		return native, nil
	}
	if !prepared.fd {
		native.Bitrate = prepared.config.Bitrate
		return native, nil
	}
	timing, nominal, data, err := resolveFDTiming(prepared.config, fdPresets80MHz[:])
	if err != nil {
		return vector.Config{}, err
	}
	if timing.ClockHz != vectorFDClockHz {
		return vector.Config{}, fmt.Errorf("%w: Vector CAN FD requires an 80 MHz clock, got %d Hz", ErrInvalidConfig, timing.ClockHz)
	}
	native.FDTiming = vector.FDTiming{
		ArbitrationBitrate: nominal,
		DataBitrate:        data,
		Arbitration: vector.BitTiming{
			SJW: timing.Nominal.SJW, TSEG1: timing.Nominal.TSEG1, TSEG2: timing.Nominal.TSEG2,
		},
		Data: vector.BitTiming{
			SJW: timing.Data.SJW, TSEG1: timing.Data.TSEG1, TSEG2: timing.Data.TSEG2,
		},
	}
	return native, nil
}
