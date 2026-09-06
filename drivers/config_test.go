package drivers

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/tomrford/gocan"
)

var qualifiedFDTiming = FDTiming{
	ClockHz: 80_000_000,
	Nominal: BitTiming{BRP: 1, TSEG1: 119, TSEG2: 40, SJW: 1},
	Data:    BitTiming{BRP: 1, TSEG1: 29, TSEG2: 10, SJW: 1},
}

func TestConfigRequiresExactlyOneTimingMode(t *testing.T) {
	channel := Channel{driver: driverPCAN, name: "PCAN-USB FD", native: 0x51, supportsFD: true}
	externalChannel := Channel{driver: driverSocketCAN, name: "can0", nativeName: "can0", supportsFD: true, external: true}
	capture := gocan.NewCapture()
	tests := []struct {
		name   string
		config Config
		wantFD bool
	}{
		{name: "classic", config: Config{ID: 1, Name: "can", Bitrate: 500_000}},
		{name: "FD", config: Config{ID: 1, Name: "can", FDTiming: qualifiedFDTiming}, wantFD: true},
		{name: "custom FD rates", config: Config{ID: 1, Name: "can", FDTiming: FDTiming{
			ClockHz: 80_000_000,
			Nominal: BitTiming{BRP: 1, TSEG1: 63, TSEG2: 16, SJW: 16}, // 1 Mbit/s
			Data:    BitTiming{BRP: 1, TSEG1: 7, TSEG2: 2, SJW: 2},    // 8 Mbit/s
		}}, wantFD: true},
		{name: "external", config: Config{ID: 1, Name: "can", External: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testChannel := channel
			if test.config.External {
				testChannel = externalChannel
			}
			got, err := prepareOpen(capture, testChannel, test.config)
			if err != nil || (got.FDTiming != (FDTiming{})) != test.wantFD || got != test.config {
				t.Fatalf("prepareOpen = %+v, %v; want unchanged config, FD=%t", got, err, test.wantFD)
			}
		})
	}

	invalid := []Config{
		{ID: 1, Name: "can"},
		{ID: 1, Name: "can", Bitrate: 500_000, External: true},
		{ID: 1, Name: "can", Bitrate: 500_000, FDTiming: qualifiedFDTiming},
		{ID: 1, Name: "can", FDTiming: qualifiedFDTiming, External: true},
		{ID: 1, Name: "can", Bitrate: 500_000, DataBitrate: 2_000_000, External: true},
		{ID: 1, Name: "can", Bitrate: 500_000, DataBitrate: 2_000_000, FDTiming: qualifiedFDTiming},
		{ID: 1, Name: "can", DataBitrate: 2_000_000, FDTiming: qualifiedFDTiming},
		{ID: 1, Name: "can", DataBitrate: 2_000_000, External: true},
	}
	for index, config := range invalid {
		if _, err := prepareOpen(capture, channel, config); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("invalid config %d error = %v, want exactly-one rejection", index, err)
		}
	}
}

func TestConfigRejectsTimingOwnedByTheWrongLayer(t *testing.T) {
	capture := gocan.NewCapture()
	programmable := Channel{driver: driverPCAN, native: 0x51, supportsFD: true}
	external := Channel{driver: driverSocketCAN, nativeName: "can0", supportsFD: true, external: true}
	if _, err := prepareOpen(capture, programmable, Config{ID: 1, Name: "can", External: true}); err == nil || !strings.Contains(err.Error(), "programmable") {
		t.Fatalf("programmable channel external timing error = %v", err)
	}
	if _, err := prepareOpen(capture, external, Config{ID: 1, Name: "can", Bitrate: 500_000}); err == nil || !strings.Contains(err.Error(), "externally") {
		t.Fatalf("external channel bitrate error = %v", err)
	}
}

func TestFDRatePresets(t *testing.T) {
	capture := gocan.NewCapture()
	for _, driver := range []driverKind{driverPCAN, driverVector} {
		t.Run(driver.String(), func(t *testing.T) {
			channel := Channel{driver: driver, supportsFD: true}
			want := []FDRatePreset{{Bitrate: 500_000, DataBitrate: 2_000_000}, {Bitrate: 500_000, DataBitrate: 4_000_000}}
			presets := channel.FDRatePresets()
			if !slices.Equal(presets, want) {
				t.Fatalf("FDRatePresets = %v; want %v", presets, want)
			}
			// A consumer editing its dropdown options must not change future
			// listings or the timing selected by Open.
			presets[0] = FDRatePreset{Bitrate: 1, DataBitrate: 2}
			presets = channel.FDRatePresets()
			if !slices.Equal(presets, want) {
				t.Fatalf("consumer changed shared presets: %v", presets)
			}
			for _, preset := range presets {
				request := Config{ID: 3, Name: "powertrain", Bitrate: preset.Bitrate, DataBitrate: preset.DataBitrate}
				got, err := prepareOpen(capture, channel, request)
				if err != nil {
					t.Fatalf("prepareOpen %v: %v", preset, err)
				}
				if got.ID != request.ID || got.Name != request.Name || got.Bitrate != 0 || got.DataBitrate != 0 || got.External {
					t.Fatalf("resolved config = %+v", got)
				}
				nominal, actualData, err := deriveFDBitrates(got.FDTiming)
				if err != nil || nominal != preset.Bitrate || actualData != preset.DataBitrate {
					t.Fatalf("resolved rates = %d/%d, %v; want %v", nominal, actualData, err, preset)
				}
				for _, phase := range []BitTiming{got.FDTiming.Nominal, got.FDTiming.Data} {
					// Both presets sample at 80% of the bit and allow SJW up to
					// the remaining 20%, as in PEAK's published 500k/2M example.
					if 5*(1+phase.TSEG1) != 4*(1+phase.TSEG1+phase.TSEG2) || phase.SJW != phase.TSEG2 {
						t.Fatalf("unexpected sample point or SJW: %+v", phase)
					}
				}
				// The resolved configuration must remain valid as explicit timing.
				if again, err := prepareOpen(capture, channel, got); err != nil || again != got {
					t.Fatalf("explicit timing = %+v, %v; want %+v", again, err, got)
				}
			}
		})
	}
}

func TestFDRatePresetsUnavailable(t *testing.T) {
	for _, channel := range []Channel{
		{},
		{driver: driverPCAN},
		{driver: driverVector},
		{driver: driverSocketCAN, supportsFD: true, external: true},
		{driver: driverPCAN, supportsFD: true, external: true},
	} {
		if presets := channel.FDRatePresets(); len(presets) != 0 {
			t.Fatalf("channel %+v lists unavailable presets: %v", channel, presets)
		}
	}
}

func TestFDRatePresetRejections(t *testing.T) {
	capture := gocan.NewCapture()
	for _, test := range []struct {
		name    string
		channel Channel
		nominal uint32
		data    uint32
		want    string
	}{
		{"missing nominal", Channel{driver: driverPCAN, supportsFD: true}, 0, 2_000_000, "requires a nominal Bitrate"},
		{"unsupported nominal", Channel{driver: driverPCAN, supportsFD: true}, 250_000, 2_000_000, "unsupported CAN FD rate pair"},
		{"unsupported data", Channel{driver: driverVector, supportsFD: true}, 500_000, 3_000_000, "unsupported CAN FD rate pair"},
		{"classic channel", Channel{driver: driverPCAN}, 500_000, 2_000_000, "does not support CAN FD"},
		{"external timing", Channel{driver: driverSocketCAN, external: true, supportsFD: true}, 500_000, 2_000_000, "configured externally"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Config{ID: 1, Name: "can", Bitrate: test.nominal, DataBitrate: test.data}
			if _, err := prepareOpen(capture, test.channel, config); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("prepareOpen error = %v; want %q", err, test.want)
			}
		})
	}
}

func TestFDTimingDerivesExactBitrates(t *testing.T) {
	nominal, data, err := deriveFDBitrates(qualifiedFDTiming)
	if err != nil {
		t.Fatalf("deriveFDBitrates: %v", err)
	}
	if nominal != 500_000 || data != 2_000_000 {
		t.Fatalf("bitrates = %d/%d, want 500000/2000000", nominal, data)
	}
}

func TestFDTimingRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		timing FDTiming
		want   string
	}{
		{name: "zero clock", timing: FDTiming{}, want: "nonzero clock"},
		{name: "zero segment", timing: FDTiming{ClockHz: 80_000_000}, want: "requires nonzero"},
		{name: "SJW exceeds TSEG2", timing: FDTiming{ClockHz: 80_000_000, Nominal: BitTiming{BRP: 1, TSEG1: 119, TSEG2: 1, SJW: 2}, Data: qualifiedFDTiming.Data}, want: "exceeds TSEG2"},
		{name: "nonintegral", timing: FDTiming{ClockHz: 80_000_001, Nominal: qualifiedFDTiming.Nominal, Data: qualifiedFDTiming.Data}, want: "integral bitrate"},
		{name: "overflow", timing: FDTiming{ClockHz: 80_000_000, Nominal: BitTiming{BRP: math.MaxUint32, TSEG1: math.MaxUint32, TSEG2: math.MaxUint32, SJW: 1}, Data: qualifiedFDTiming.Data}, want: "overflows"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := deriveFDBitrates(test.timing); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("deriveFDBitrates error = %v, want %q", err, test.want)
			}
		})
	}
}
