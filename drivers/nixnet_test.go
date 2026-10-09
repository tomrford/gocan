package drivers

import (
	"github.com/tomrford/gocan"
	"testing"
)

func TestNIXNETConfiguration(t *testing.T) {
	channel := Channel{driver: driverNIXNET, nativeName: "CAN5", supportsFD: true, supportsTermination: true}
	if channel.Identifier() != "nixnet:CAN5" || channel.Driver() != "nixnet" || !channel.SupportsTermination() {
		t.Fatalf("wrong channel metadata: %+v", channel)
	}
	for _, concurrent := range []bool{false, true} {
		config := Config{ID: 1, Name: "NI", Bitrate: 500000, NIXNETConcurrentIO: concurrent}
		got, err := prepareOpen(gocan.NewCapture(), channel, config)
		if err != nil || got != config {
			t.Fatalf("NI concurrent mode %t: got %+v, %v", concurrent, got, err)
		}
		for _, driver := range []driverKind{driverPCAN, driverVector, driverSocketCAN} {
			other := Channel{driver: driver, external: driver == driverSocketCAN}
			config.External = other.external
			config.Bitrate = 500000
			if config.External {
				config.Bitrate = 0
			}
			_, err := prepareOpen(gocan.NewCapture(), other, config)
			if (err != nil) != concurrent {
				t.Fatalf("%s concurrent mode %t: %v", other.Driver(), concurrent, err)
			}
		}
	}
	for _, preset := range channel.fdPresets() {
		cfg, err := prepareOpen(gocan.NewCapture(), channel, Config{ID: 1, Name: "NI", Bitrate: preset.rate.Bitrate, DataBitrate: preset.rate.DataBitrate, Termination: TerminationOn})
		if err != nil {
			t.Fatal(err)
		}
		nominal, data, err := nixnetFDTiming(cfg.FDTiming)
		if err != nil {
			t.Fatal(err)
		}
		wantData := uint64(0x801000a0032e33)
		if preset.rate.DataBitrate == 4000000 {
			wantData = 0x800800a0032611
		}
		if nominal != 0x19a00f3e0f || data != wantData {
			t.Fatalf("encoded timing %x/%x want %x/%x", nominal, data, uint64(0x19a00f3e0f), wantData)
		}
	}
	equivalent := FDTiming{ClockHz: 80_000_000, Nominal: BitTiming{BRP: 2, TSEG1: 63, TSEG2: 16, SJW: 16}, Data: BitTiming{BRP: 2, TSEG1: 15, TSEG2: 4, SJW: 4}}
	one, two, err := nixnetFDTiming(equivalent)
	if err != nil || one != 0x19a00f3e0f || two != 0x801000a0032e33 {
		t.Fatalf("equivalent clock translation %x/%x: %v", one, two, err)
	}
	for _, bad := range []FDTiming{
		qualifiedFDTiming,
		{ClockHz: 40_000_000, Nominal: BitTiming{BRP: 1, TSEG1: 256, TSEG2: 128, SJW: 129}, Data: BitTiming{BRP: 1, TSEG1: 15, TSEG2: 4, SJW: 4}},
		{ClockHz: 40_000_000, Nominal: BitTiming{BRP: 1, TSEG1: 63, TSEG2: 16, SJW: 16}, Data: BitTiming{BRP: 1, TSEG1: 33, TSEG2: 6, SJW: 1}},
	} {
		if _, _, err := nixnetFDTiming(bad); err == nil {
			t.Fatalf("accepted invalid NI timing: %+v", bad)
		}
	}
	for _, term := range []Termination{TerminationDefault, TerminationOff, TerminationOn} {
		if _, err := prepareOpen(gocan.NewCapture(), channel, Config{ID: 1, Name: "NI", Bitrate: 500000, Termination: term}); err != nil {
			t.Fatal(err)
		}
	}
	channel.supportsTermination = false
	for _, term := range []Termination{TerminationOff, TerminationOn, Termination(3)} {
		if _, err := prepareOpen(gocan.NewCapture(), channel, Config{ID: 1, Name: "NI", Bitrate: 500000, Termination: term}); err == nil {
			t.Fatalf("accepted unsupported termination %d", term)
		}
	}
}
