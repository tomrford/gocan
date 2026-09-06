package drivers

import (
	"errors"
	"fmt"
	"math"

	"github.com/tomrford/gocan"
)

// Config names an opened bus and selects how its bit timing is configured.
// Set either Bitrate (optionally with DataBitrate), FDTiming, or External.
type Config struct {
	// ID is the one-based trace channel assigned to the bus.
	ID gocan.BusID
	// Name is the human-readable bus name.
	Name string
	// Bitrate selects classical CAN, or the nominal CAN FD rate when
	// DataBitrate is set, in bits per second. For classical CAN, PCAN accepts
	// only its predefined rates, with fractional rates rounded to the nearest
	// bit per second (for example 83_333).
	Bitrate uint32
	// DataBitrate selects CAN FD using built-in timing on PCAN and Vector.
	// Channel.FDRatePresets lists the available rate pairs. The presets use
	// an 80% sample point in both phases.
	// Zero selects classical CAN when Bitrate is set. Use FDTiming instead
	// when the network requires other rates or specific timing.
	DataBitrate uint32
	// FDTiming selects programmable CAN FD with exact bit timing.
	FDTiming FDTiming
	// External selects timing already configured by the operating system.
	External bool
}

// FDRatePreset is a gocan-provided nominal/data rate pair in bits per second.
// Copy its fields to Config to open a channel using the preset's timing.
type FDRatePreset struct {
	Bitrate     uint32
	DataBitrate uint32
}

type fdPreset struct {
	rate   FDRatePreset
	timing FDTiming
}

// PEAK's InitializeFD example defines 500k/2M at 80% sample points:
// https://www.peak-system.com/documentation/API/PCAN-Basic.Net/html/eebb7d25-f978-60f2-54b8-5b126db9dff5.htm
// Halving the data prescaler gives 4M with the same sample point. Both presets
// also satisfy Vector XLcanFdConf's segment limits and 80 MHz clock/prescaler
// equation (XL Driver Library Manual 20.30, section 5.4.1).
var fdPresets = [...]fdPreset{
	{
		rate: FDRatePreset{Bitrate: 500_000, DataBitrate: 2_000_000},
		timing: FDTiming{
			ClockHz: 80_000_000,
			Nominal: BitTiming{BRP: 2, TSEG1: 63, TSEG2: 16, SJW: 16},
			Data:    BitTiming{BRP: 2, TSEG1: 15, TSEG2: 4, SJW: 4},
		},
	},
	{
		rate: FDRatePreset{Bitrate: 500_000, DataBitrate: 4_000_000},
		timing: FDTiming{
			ClockHz: 80_000_000,
			Nominal: BitTiming{BRP: 2, TSEG1: 63, TSEG2: 16, SJW: 16},
			Data:    BitTiming{BRP: 1, TSEG1: 15, TSEG2: 4, SJW: 4},
		},
	},
}

// FDRatePresets returns an owned list of gocan-provided rate presets for this
// channel, ordered by nominal then data bitrate. These are configuration
// choices, not an exhaustive list of rates the hardware supports or a guarantee
// of communication on a particular network. An empty list means no presets are
// available; use SupportsFD to check hardware capability. Config.FDTiming allows
// custom timing on programmable channels.
func (channel Channel) FDRatePresets() []FDRatePreset {
	var rates []FDRatePreset
	for _, preset := range channel.fdPresets() {
		rates = append(rates, preset.rate)
	}
	return rates
}

func (channel Channel) fdPresets() []fdPreset {
	if !channel.supportsFD || channel.external || (channel.driver != driverPCAN && channel.driver != driverVector) {
		return nil
	}
	return fdPresets[:]
}

// FDTiming defines exact CAN FD arbitration- and data-phase bit timing.
type FDTiming struct {
	// ClockHz is the controller clock in hertz.
	ClockHz uint32
	// Nominal configures the arbitration phase.
	Nominal BitTiming
	// Data configures the data phase.
	Data BitTiming
}

// BitTiming defines one CAN bit as a prescaler and timing segments.
type BitTiming struct {
	// BRP is the bit-rate prescaler.
	BRP uint32
	// TSEG1 is the first timing segment in time quanta.
	TSEG1 uint32
	// TSEG2 is the second timing segment in time quanta.
	TSEG2 uint32
	// SJW is the synchronization jump width in time quanta.
	SJW uint32
}

// prepareOpen validates the open contract and resolves rate presets to explicit
// timing before passing the configuration to a native driver.
func prepareOpen(capture *gocan.Capture, channel Channel, config Config) (Config, error) {
	switch {
	case capture == nil:
		return Config{}, errors.New("physical CAN bus requires a capture")
	case channel.driver == driverUnknown:
		return Config{}, errors.New("physical CAN bus requires a channel returned by Discover")
	case config.ID == 0:
		return Config{}, errors.New("physical CAN bus requires an ID")
	case config.Name == "":
		return Config{}, errors.New("physical CAN bus requires a name")
	}

	fd := config.FDTiming != (FDTiming{})
	choices := 0
	if config.Bitrate != 0 || config.DataBitrate != 0 {
		choices++
	}
	if fd {
		choices++
	}
	if config.External {
		choices++
	}
	if choices != 1 {
		return Config{}, errors.New("physical CAN bus requires exactly one of Bitrate (optionally with DataBitrate), FDTiming, and External")
	}

	if channel.external && !config.External {
		return Config{}, fmt.Errorf("%s is configured externally; set Config.External", channel.Identifier())
	}
	if !channel.external && config.External {
		return Config{}, fmt.Errorf("%s requires programmable bit timing", channel.Identifier())
	}
	if config.DataBitrate != 0 && config.Bitrate == 0 {
		return Config{}, errors.New("CAN FD DataBitrate requires a nominal Bitrate")
	}
	if fd || config.DataBitrate != 0 {
		if !channel.supportsFD {
			return Config{}, fmt.Errorf("%s does not support CAN FD", channel.Identifier())
		}
		if config.DataBitrate != 0 {
			var err error
			config.FDTiming, err = fdRatePreset(channel, config.Bitrate, config.DataBitrate)
			if err != nil {
				return Config{}, err
			}
			config.Bitrate, config.DataBitrate = 0, 0
		}
		if _, _, err := deriveFDBitrates(config.FDTiming); err != nil {
			return Config{}, err
		}
	}
	return config, nil
}

func fdRatePreset(channel Channel, nominal, data uint32) (FDTiming, error) {
	for _, preset := range channel.fdPresets() {
		if preset.rate.Bitrate == nominal && preset.rate.DataBitrate == data {
			return preset.timing, nil
		}
	}
	return FDTiming{}, fmt.Errorf("unsupported CAN FD rate pair %d/%d bits/s; use FDTiming for custom timing", nominal, data)
}

func deriveFDBitrates(timing FDTiming) (uint32, uint32, error) {
	if timing.ClockHz == 0 {
		return 0, 0, errors.New("CAN FD timing requires a nonzero clock")
	}
	nominal, err := deriveBitrate(timing.ClockHz, "nominal", timing.Nominal)
	if err != nil {
		return 0, 0, err
	}
	data, err := deriveBitrate(timing.ClockHz, "data", timing.Data)
	if err != nil {
		return 0, 0, err
	}
	return nominal, data, nil
}

func deriveBitrate(clock uint32, phase string, timing BitTiming) (uint32, error) {
	if timing.BRP == 0 || timing.TSEG1 == 0 || timing.TSEG2 == 0 || timing.SJW == 0 {
		return 0, fmt.Errorf("CAN FD %s timing requires nonzero BRP, TSEG1, TSEG2, and SJW", phase)
	}
	if timing.SJW > timing.TSEG2 {
		return 0, fmt.Errorf("CAN FD %s SJW %d exceeds TSEG2 %d", phase, timing.SJW, timing.TSEG2)
	}
	quanta := uint64(1) + uint64(timing.TSEG1) + uint64(timing.TSEG2)
	if quanta > math.MaxUint64/uint64(timing.BRP) {
		return 0, fmt.Errorf("CAN FD %s timing denominator overflows", phase)
	}
	denominator := uint64(timing.BRP) * quanta
	if uint64(clock)%denominator != 0 {
		return 0, fmt.Errorf("CAN FD %s timing does not divide %d Hz into an integral bitrate", phase, clock)
	}
	return uint32(uint64(clock) / denominator), nil
}
