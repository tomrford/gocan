//go:build windows && amd64

package drivers

import (
	"context"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/vector"
)

func openVector(ctx context.Context, capture *gocan.Capture, channel Channel, config Config, mode configMode) (gocan.Bus, error) {
	native, err := nativeVectorConfig(channel, config, mode)
	if err != nil {
		return nil, err
	}
	return vector.Open(ctx, capture, native)
}

func nativeVectorConfig(channel Channel, config Config, mode configMode) (vector.Config, error) {
	native := vector.Config{
		ID:           config.ID,
		Name:         config.Name,
		ChannelIndex: vector.ChannelIndex(channel.native),
	}
	switch mode {
	case modeClassic:
		native.Bitrate = config.Bitrate
	case modeFD:
		nominal, data, _ := deriveFDBitrates(config.FDTiming)
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
	default:
		return vector.Config{}, invalidModeError(mode)
	}
	return native, nil
}
