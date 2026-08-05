package drivers

import (
	"math"
	"reflect"
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
		want   configMode
	}{
		{name: "classic", config: Config{ID: 1, Name: "can", Bitrate: 500_000}, want: modeClassic},
		{name: "FD", config: Config{ID: 1, Name: "can", FDTiming: qualifiedFDTiming}, want: modeFD},
		{name: "external", config: Config{ID: 1, Name: "can", External: true}, want: modeExternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testChannel := channel
			if test.want == modeExternal {
				testChannel = externalChannel
			}
			got, err := validateOpen(capture, testChannel, test.config)
			if err != nil || got != test.want {
				t.Fatalf("validateOpen = %d, %v; want %d", got, err, test.want)
			}
		})
	}

	invalid := []Config{
		{ID: 1, Name: "can"},
		{ID: 1, Name: "can", Bitrate: 500_000, External: true},
		{ID: 1, Name: "can", Bitrate: 500_000, FDTiming: qualifiedFDTiming},
		{ID: 1, Name: "can", FDTiming: qualifiedFDTiming, External: true},
	}
	for index, config := range invalid {
		if _, err := validateOpen(capture, channel, config); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("invalid config %d error = %v, want exactly-one rejection", index, err)
		}
	}
}

func TestConfigRejectsTimingOwnedByTheWrongLayer(t *testing.T) {
	capture := gocan.NewCapture()
	programmable := Channel{driver: driverPCAN, native: 0x51, supportsFD: true}
	external := Channel{driver: driverSocketCAN, nativeName: "can0", supportsFD: true, external: true}
	if _, err := validateOpen(capture, programmable, Config{ID: 1, Name: "can", External: true}); err == nil || !strings.Contains(err.Error(), "programmable") {
		t.Fatalf("programmable channel external timing error = %v", err)
	}
	if _, err := validateOpen(capture, external, Config{ID: 1, Name: "can", Bitrate: 500_000}); err == nil || !strings.Contains(err.Error(), "externally") {
		t.Fatalf("external channel bitrate error = %v", err)
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

func TestChannelIsOpaqueAndDistinguishable(t *testing.T) {
	typeOfChannel := reflect.TypeFor[Channel]()
	for index := range typeOfChannel.NumField() {
		if typeOfChannel.Field(index).IsExported() {
			t.Fatalf("Channel field %s is exported", typeOfChannel.Field(index).Name)
		}
	}
	first := Channel{driver: driverPCAN, name: "PCAN-USB FD", native: 0x51, supportsFD: true}
	second := Channel{driver: driverPCAN, name: "PCAN-USB FD", native: 0x52, supportsFD: true}
	if first.Identifier() == second.Identifier() || first == second {
		t.Fatalf("channels are not distinguishable: %q and %q", first.Identifier(), second.Identifier())
	}
	if first.Driver() != "pcan" || first.Name() != "PCAN-USB FD" || !first.SupportsFD() || first.ExternallyConfigured() {
		t.Fatalf("unexpected PCAN channel metadata: %#v", first)
	}
	external := Channel{driver: driverSocketCAN, name: "can0", nativeName: "can0", supportsFD: true, external: true}
	if external.Identifier() != "socketcan:can0" || !external.ExternallyConfigured() {
		t.Fatalf("unexpected SocketCAN channel metadata: %#v", external)
	}
}
