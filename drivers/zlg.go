package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/zlg"
)

var zlgDriver = driverDescriptor{
	name: "zlg",
	formatIdentifier: func(channel Channel) string {
		deviceType, device, index := zlgSelector(channel.native)
		return fmt.Sprintf("zlg:%d:%d:%d", deviceType, device, index)
	},
	fdRatePresets: func() []FDRatePreset {
		rates := make([]FDRatePreset, len(zlg.FDRates))
		for i, rate := range zlg.FDRates {
			rates[i] = FDRatePreset{Bitrate: rate.Bitrate, DataBitrate: rate.DataBitrate}
		}
		return rates
	},
	prepare: func(channel Channel, prepared preparedConfig) (preparedOpen, error) {
		native, err := nativeZLGConfig(channel, prepared)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, capture *gocan.Capture) (gocan.Bus, error) {
			return openZLG(ctx, capture, native)
		}, nil
	},
	discover: discoverZLG,
}

func zlgChannel(name string, deviceType, device, channel uint32, supportsFD bool) Channel {
	return Channel{
		name:                name,
		native:              zlgNative(deviceType, device, channel),
		supportsFD:          supportsFD,
		supportsTermination: supportsFD,
	}
}

func nativeZLGConfig(channel Channel, prepared preparedConfig) (zlg.Config, error) {
	if prepared.config.Vendor != nil {
		return zlg.Config{}, rejectVendorOptions(channel, prepared.config.Vendor)
	}
	if prepared.config.FDTiming != (FDTiming{}) {
		return zlg.Config{}, fmt.Errorf("%w: %s accepts only its CAN FD rate presets", ErrInvalidConfig, channel.Identifier())
	}
	deviceType, device, index := zlgSelector(channel.native)
	native := zlg.Config{
		ID:          prepared.config.ID,
		Name:        prepared.config.Name,
		DeviceType:  deviceType,
		Device:      device,
		Channel:     index,
		Bitrate:     prepared.config.Bitrate,
		DataBitrate: prepared.config.DataBitrate,
		Termination: prepared.termination,
	}
	if err := zlg.ValidateConfig(native); err != nil {
		return zlg.Config{}, err
	}
	return native, nil
}

// A ZLG native selector packs the device type, device index and channel.
func zlgNative(deviceType, device, channel uint32) uint64 {
	return uint64(deviceType)<<48 | uint64(device&0xffff)<<32 | uint64(channel)
}

func zlgSelector(native uint64) (deviceType, device, channel uint32) {
	return uint32(native >> 48), uint32(native>>32) & 0xffff, uint32(native)
}
