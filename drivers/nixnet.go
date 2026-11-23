package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/nixnet"
)

// NIXNETOptions configures NI-XNET-specific bus behavior via Config.Vendor.
type NIXNETOptions struct {
	// ConcurrentIO lets NI-XNET read frames and controller state without
	// holding the transmit lock to reduce send delays during slow native reads.
	// Capture ordering can then place an earlier-fetched reply after a newer
	// request; use it only for fully awaited exchanges without stale or
	// unsolicited replies on that receive address. Fixed at Open.
	ConcurrentIO bool
}

func (NIXNETOptions) vendorOptions() {}

var nixnetDriver = driverDescriptor{
	name: "nixnet",
	formatIdentifier: func(channel Channel) string {
		return "nixnet:" + channel.nativeName
	},
	fdRatePresets: func() []FDRatePreset {
		return presetRates(nixnetFDPresets[:])
	},
	prepare: func(channel Channel, prepared preparedConfig) (preparedOpen, error) {
		native, err := nativeNIXNETConfig(channel, prepared)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, capture *gocan.Capture) (gocan.Bus, error) {
			return openNIXNET(ctx, capture, native)
		}, nil
	},
	discover: discoverNIXNET,
}

func nativeNIXNETConfig(channel Channel, prepared preparedConfig) (nixnet.Config, error) {
	var opts NIXNETOptions
	switch vendor := prepared.config.Vendor.(type) {
	case nil:
	case NIXNETOptions:
		opts = vendor
	case *NIXNETOptions:
		if vendor == nil {
			return nixnet.Config{}, fmt.Errorf("%w: %s received nil *NIXNETOptions", ErrInvalidConfig, channel.Identifier())
		}
		opts = *vendor
	default:
		return nixnet.Config{}, rejectVendorOptions(channel, prepared.config.Vendor)
	}

	native := nixnet.Config{
		ID:           prepared.config.ID,
		Name:         prepared.config.Name,
		Interface:    channel.nativeName,
		Baud:         uint64(prepared.config.Bitrate),
		FD:           prepared.fd,
		Termination:  prepared.termination,
		ConcurrentIO: opts.ConcurrentIO,
	}
	if prepared.fd {
		timing, _, _, err := resolveFDTiming(prepared.config, nixnetFDPresets[:])
		if err != nil {
			return nixnet.Config{}, err
		}
		nominal, data, err := nixnetFDTiming(timing)
		if err != nil {
			return nixnet.Config{}, err
		}
		native.Baud = nominal
		native.FDBaud = data
	}
	return native, nil
}

// NI-XNET uses 25 ns quanta (40 MHz). Keep the public clock/prescaler
// representation, translating equivalent quanta exactly rather than rounding.
var nixnetFDPresets = [...]fdPreset{
	{rate: FDRatePreset{Bitrate: 500_000, DataBitrate: 2_000_000}, timing: FDTiming{ClockHz: 40_000_000, Nominal: BitTiming{BRP: 1, TSEG1: 63, TSEG2: 16, SJW: 16}, Data: BitTiming{BRP: 1, TSEG1: 15, TSEG2: 4, SJW: 4}}},
	{rate: FDRatePreset{Bitrate: 500_000, DataBitrate: 4_000_000}, timing: FDTiming{ClockHz: 40_000_000, Nominal: BitTiming{BRP: 1, TSEG1: 63, TSEG2: 16, SJW: 16}, Data: BitTiming{BRP: 1, TSEG1: 7, TSEG2: 2, SJW: 2}}},
}

// nixnetFDTiming encodes a timing validated by resolveFDTiming.
func nixnetFDTiming(timing FDTiming) (uint64, uint64, error) {
	nominal, err := nixnetBitTiming(timing.ClockHz, timing.Nominal, false)
	if err != nil {
		return 0, 0, err
	}
	data, err := nixnetBitTiming(timing.ClockHz, timing.Data, true)
	if err != nil {
		return 0, 0, err
	}
	return nominal, data, nil
}

func nixnetBitTiming(clock uint32, timing BitTiming, data bool) (uint64, error) {
	nanos := uint64(timing.BRP) * 1_000_000_000
	if nanos%uint64(clock) != 0 {
		return 0, fmt.Errorf("%w: NI-XNET timing requires integral time quanta in nanoseconds", ErrInvalidConfig)
	}
	tq := nanos / uint64(clock)
	maxTQ, maxTSEG1, maxTSEG2, maxSJW := uint64(12800), uint32(256), uint32(128), uint32(128)
	minTSEG1 := uint32(2)
	phase := "nominal"
	if data {
		phase = "data"
		maxTQ, maxTSEG1, maxTSEG2, maxSJW = 800, 32, 16, 16
		minTSEG1 = 1
	}
	if tq < 25 || tq > maxTQ || tq%25 != 0 || timing.TSEG1 < minTSEG1 || timing.TSEG1 > maxTSEG1 || timing.TSEG2 == 0 || timing.TSEG2 > maxTSEG2 || timing.SJW == 0 || timing.SJW > maxSJW || timing.SJW > timing.TSEG2 {
		return 0, fmt.Errorf("%w: NI-XNET timing exceeds %s phase limits or requires a quantum other than multiples of 25 ns", ErrInvalidConfig, phase)
	}
	// Encode nxPropSession_IntfBaudRate64 and IntfCanFdBaudRate64.
	if data {
		encoded := uint64(0xa0000000) | tq<<13 | uint64(timing.TSEG1-1)<<8 | uint64(timing.TSEG2-1)<<4 | uint64(timing.SJW-1)
		// TDCO uses controller clock periods (25 ns), not prescaled quanta.
		// Enable measured loop-delay compensation for data rates above 1M.
		// https://knowledge.ni.com/KnowledgeArticleDetails?id=kA0VU0000002kGT0AY
		if tq*(1+uint64(timing.TSEG1)+uint64(timing.TSEG2)) < 1000 {
			offset := tq * (1 + uint64(timing.TSEG1)) / 25
			encoded |= uint64(1)<<55 | offset<<40
		}
		return encoded, nil
	}
	return 0xa0000000 | tq<<32 | uint64(timing.SJW-1)<<16 | uint64(timing.TSEG1-1)<<8 | uint64(timing.TSEG2-1), nil
}
