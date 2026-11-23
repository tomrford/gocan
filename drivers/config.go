package drivers

import (
	"fmt"
	"math"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/drivercfg"
)

// ErrInvalidConfig identifies an invalid or unsupported channel or Config
// rejected before opening hardware.
var ErrInvalidConfig = drivercfg.ErrInvalidConfig

// Config names an opened bus and selects how its bit timing is configured.
// Set either Bitrate (optionally with DataBitrate), FDTiming, or External.
type Config struct {
	// ID is the one-based trace channel assigned to the bus.
	ID gocan.BusID
	// Name is the human-readable bus name.
	Name string
	// Bitrate selects classical CAN, or the nominal CAN FD rate when
	// DataBitrate is set, in bits per second. Some drivers accept only
	// predefined classical bitrates.
	Bitrate uint32
	// DataBitrate selects CAN FD using driver-supported rate presets.
	// Channel.FDRatePresets lists the available rate pairs. Zero selects
	// classical CAN when Bitrate is set. Use FDTiming instead when the network
	// requires other rates or custom timing on programmable channels.
	DataBitrate uint32
	// FDTiming selects programmable CAN FD with exact bit timing.
	FDTiming FDTiming
	// External selects timing already configured outside gocan, on channels
	// whose ExternallyConfigured method returns true. On shared channels, Open
	// verifies the channel is on the bus before joining but cannot prevent the
	// owning application from closing or changing timing during the open.
	External bool
	// Termination selects switchable bus termination on supported channels.
	// Default leaves the driver default; unsupported explicit choices are rejected.
	Termination Termination
	// Vendor carries optional driver-specific settings, such as NIXNETOptions
	// or a non-nil pointer to it. Nil leaves the driver's defaults in place.
	// Options for another driver, typed-nil pointers, or unknown option types
	// fail with ErrInvalidConfig before any native driver call.
	Vendor VendorOptions
}

// VendorOptions carries driver-specific settings for Config.Vendor.
// Concrete option types such as NIXNETOptions are defined in this package;
// unknown implementations are rejected with ErrInvalidConfig.
type VendorOptions interface {
	vendorOptions()
}

// Termination selects the physical bus termination resistor.
type Termination uint8

const (
	// TerminationDefault leaves the native driver's default setting.
	TerminationDefault Termination = iota
	// TerminationOff disables the termination resistor.
	TerminationOff
	// TerminationOn enables the termination resistor.
	TerminationOn
)

func (termination Termination) toInternal() (drivercfg.Termination, error) {
	switch termination {
	case TerminationDefault:
		return drivercfg.TerminationDefault, nil
	case TerminationOff:
		return drivercfg.TerminationOff, nil
	case TerminationOn:
		return drivercfg.TerminationOn, nil
	default:
		return 0, fmt.Errorf("%w: invalid CAN termination choice %d", ErrInvalidConfig, termination)
	}
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
var fdPresets80MHz = [...]fdPreset{
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

func presetRates(presets []fdPreset) []FDRatePreset {
	rates := make([]FDRatePreset, len(presets))
	for i, preset := range presets {
		rates[i] = preset.rate
	}
	return rates
}

// FDRatePresets returns an owned list of gocan-provided rate presets for this
// channel, ordered by nominal then data bitrate. These are configuration
// choices, not an exhaustive list of rates the hardware supports or a guarantee
// of communication on a particular network. An empty list means no presets are
// available; use SupportsFD to check hardware capability. Config.FDTiming allows
// custom timing on programmable channels.
func (channel Channel) FDRatePresets() []FDRatePreset {
	if channel.driver == nil || !channel.supportsFD || channel.external || channel.driver.fdRatePresets == nil {
		return nil
	}
	return channel.driver.fdRatePresets()
}

// FDTiming defines exact CAN FD arbitration- and data-phase bit timing.
// Native drivers may enforce additional hardware clock or segment limits when
// opening the channel.
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

type preparedConfig struct {
	config      Config
	termination drivercfg.Termination
	fd          bool
}

func prepareOpen(capture *gocan.Capture, channel Channel, config Config) (preparedOpen, error) {
	switch {
	case capture == nil:
		return nil, fmt.Errorf("%w: physical CAN bus requires a capture", ErrInvalidConfig)
	case channel.driver == nil:
		return nil, fmt.Errorf("%w: physical CAN bus requires a channel returned by Discover", ErrInvalidConfig)
	case config.ID == 0:
		return nil, fmt.Errorf("%w: physical CAN bus requires an ID", ErrInvalidConfig)
	case config.Name == "":
		return nil, fmt.Errorf("%w: physical CAN bus requires a name", ErrInvalidConfig)
	}

	termination, err := config.Termination.toInternal()
	if err != nil {
		return nil, err
	}
	if termination != drivercfg.TerminationDefault && !channel.supportsTermination {
		return nil, fmt.Errorf("%w: %s does not support switchable termination", ErrInvalidConfig, channel.Identifier())
	}

	hasFDTiming := config.FDTiming != (FDTiming{})
	choices := 0
	if config.Bitrate != 0 || config.DataBitrate != 0 {
		choices++
	}
	if hasFDTiming {
		choices++
	}
	if config.External {
		choices++
	}
	if choices != 1 {
		return nil, fmt.Errorf("%w: physical CAN bus requires exactly one of Bitrate (optionally with DataBitrate), FDTiming, and External", ErrInvalidConfig)
	}

	if channel.external && !config.External {
		return nil, fmt.Errorf("%w: %s is configured externally; set Config.External", ErrInvalidConfig, channel.Identifier())
	}
	if !channel.external && config.External {
		return nil, fmt.Errorf("%w: %s requires programmable bit timing", ErrInvalidConfig, channel.Identifier())
	}
	if config.DataBitrate != 0 && config.Bitrate == 0 {
		return nil, fmt.Errorf("%w: CAN FD DataBitrate requires a nominal Bitrate", ErrInvalidConfig)
	}

	fd := hasFDTiming || config.DataBitrate != 0
	if fd && !channel.supportsFD {
		return nil, fmt.Errorf("%w: %s does not support CAN FD", ErrInvalidConfig, channel.Identifier())
	}

	return channel.driver.prepare(channel, preparedConfig{
		config:      config,
		termination: termination,
		fd:          fd,
	})
}

func rejectVendorOptions(channel Channel, vendor VendorOptions) error {
	return fmt.Errorf("%w: %s does not accept vendor options %T", ErrInvalidConfig, channel.Identifier(), vendor)
}

func resolveFDTiming(config Config, presets []fdPreset) (FDTiming, uint32, uint32, error) {
	timing := config.FDTiming
	if config.DataBitrate != 0 {
		var matched bool
		for _, preset := range presets {
			if preset.rate.Bitrate == config.Bitrate && preset.rate.DataBitrate == config.DataBitrate {
				timing = preset.timing
				matched = true
				break
			}
		}
		if !matched {
			return FDTiming{}, 0, 0, fmt.Errorf("%w: unsupported CAN FD rate pair %d/%d bits/s; use FDTiming for custom timing", ErrInvalidConfig, config.Bitrate, config.DataBitrate)
		}
	}
	nominal, data, err := deriveFDBitrates(timing)
	if err != nil {
		return FDTiming{}, 0, 0, err
	}
	return timing, nominal, data, nil
}

func deriveFDBitrates(timing FDTiming) (uint32, uint32, error) {
	if timing.ClockHz == 0 {
		return 0, 0, fmt.Errorf("%w: CAN FD timing requires a nonzero clock", ErrInvalidConfig)
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
		return 0, fmt.Errorf("%w: CAN FD %s timing requires nonzero BRP, TSEG1, TSEG2, and SJW", ErrInvalidConfig, phase)
	}
	if timing.SJW > timing.TSEG2 {
		return 0, fmt.Errorf("%w: CAN FD %s SJW %d exceeds TSEG2 %d", ErrInvalidConfig, phase, timing.SJW, timing.TSEG2)
	}
	quanta := uint64(1) + uint64(timing.TSEG1) + uint64(timing.TSEG2)
	if quanta > math.MaxUint64/uint64(timing.BRP) {
		return 0, fmt.Errorf("%w: CAN FD %s timing denominator overflows", ErrInvalidConfig, phase)
	}
	denominator := uint64(timing.BRP) * quanta
	if uint64(clock)%denominator != 0 {
		return 0, fmt.Errorf("%w: CAN FD %s timing does not divide %d Hz into an integral bitrate", ErrInvalidConfig, phase, clock)
	}
	return uint32(uint64(clock) / denominator), nil
}
