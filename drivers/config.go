package drivers

import (
	"errors"
	"fmt"
	"math"

	"github.com/tomrford/gocan"
)

// Config names an opened bus and selects how its bit timing is configured.
// Set exactly one of Bitrate, FDTiming, and External.
type Config struct {
	// ID is the one-based trace channel assigned to the bus.
	ID gocan.BusID
	// Name is the human-readable bus name.
	Name string
	// Bitrate selects programmable classical CAN in bits per second.
	Bitrate uint32
	// FDTiming selects programmable CAN FD with exact bit timing.
	FDTiming FDTiming
	// External selects timing already configured by the operating system.
	External bool
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

// validateOpen checks the generic open contract once for every driver and
// reports whether CAN FD timing was selected.
func validateOpen(capture *gocan.Capture, channel Channel, config Config) (bool, error) {
	switch {
	case capture == nil:
		return false, errors.New("physical CAN bus requires a capture")
	case channel.driver == driverUnknown:
		return false, errors.New("physical CAN bus requires a channel returned by Discover")
	case config.ID == 0:
		return false, errors.New("physical CAN bus requires an ID")
	case config.Name == "":
		return false, errors.New("physical CAN bus requires a name")
	}

	fd := config.FDTiming != (FDTiming{})
	choices := 0
	if config.Bitrate != 0 {
		choices++
	}
	if fd {
		choices++
	}
	if config.External {
		choices++
	}
	if choices != 1 {
		return false, errors.New("physical CAN bus requires exactly one of Bitrate, FDTiming, and External")
	}

	if channel.external && !config.External {
		return false, fmt.Errorf("%s is configured externally; set Config.External", channel.Identifier())
	}
	if !channel.external && config.External {
		return false, fmt.Errorf("%s requires programmable bit timing", channel.Identifier())
	}
	if fd {
		if !channel.supportsFD {
			return false, fmt.Errorf("%s does not support CAN FD", channel.Identifier())
		}
		if _, _, err := deriveFDBitrates(config.FDTiming); err != nil {
			return false, err
		}
	}
	return fd, nil
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
