//go:build windows && amd64

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/vector"
)

const vectorFDClockHz = 80_000_000

func openVector(ctx context.Context, capture *gocan.Capture, channel Channel, config Config, fd bool) (gocan.Bus, error) {
	native, err := nativeVectorConfig(channel, config, fd)
	if err != nil {
		return nil, err
	}
	return vector.Open(ctx, capture, native)
}

func nativeVectorConfig(channel Channel, config Config, fd bool) (vector.Config, error) {
	native := vector.Config{
		ID:           config.ID,
		Name:         config.Name,
		ChannelIndex: vector.ChannelIndex(channel.native),
	}
	if !fd {
		native.Bitrate = config.Bitrate
		return native, nil
	}
	if config.FDTiming.ClockHz != vectorFDClockHz {
		return vector.Config{}, fmt.Errorf("Vector CAN FD requires an 80 MHz clock, got %d Hz", config.FDTiming.ClockHz)
	}
	nominal, data, err := deriveFDBitrates(config.FDTiming)
	if err != nil {
		return vector.Config{}, err
	}
	native.FDTiming = vector.FDTiming{
		ArbitrationBitrate: nominal,
		DataBitrate:        data,
		Arbitration: vector.BitTiming{
			SJW: config.FDTiming.Nominal.SJW, TSEG1: config.FDTiming.Nominal.TSEG1, TSEG2: config.FDTiming.Nominal.TSEG2,
		},
		Data: vector.BitTiming{
			SJW: config.FDTiming.Data.SJW, TSEG1: config.FDTiming.Data.TSEG1, TSEG2: config.FDTiming.Data.TSEG2,
		},
	}
	return native, nil
}
