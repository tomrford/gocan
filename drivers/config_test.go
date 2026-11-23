package drivers_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers"
)

var qualifiedFDTiming = drivers.FDTiming{
	ClockHz: 80_000_000,
	Nominal: drivers.BitTiming{BRP: 1, TSEG1: 119, TSEG2: 40, SJW: 1},
	Data:    drivers.BitTiming{BRP: 1, TSEG1: 29, TSEG2: 10, SJW: 1},
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestOpenAcceptsValidConfigurations(t *testing.T) {
	ctx := canceledContext()
	capture := gocan.NewCapture()

	tests := []struct {
		name    string
		channel drivers.Channel
		config  drivers.Config
	}{
		{
			name:    "PCAN classic",
			channel: drivers.PCANTestChannel(true),
			config:  drivers.Config{ID: 1, Name: "pcan", Bitrate: 500_000},
		},
		{
			name:    "PCAN custom FD timing",
			channel: drivers.PCANTestChannel(true),
			config:  drivers.Config{ID: 1, Name: "pcan", FDTiming: qualifiedFDTiming},
		},
		{
			name:    "Vector classic",
			channel: drivers.VectorTestChannel(true, false),
			config:  drivers.Config{ID: 1, Name: "vector", Bitrate: 500_000},
		},
		{
			name:    "Vector custom FD timing",
			channel: drivers.VectorTestChannel(true, false),
			config:  drivers.Config{ID: 1, Name: "vector", FDTiming: qualifiedFDTiming},
		},
		{
			name:    "Vector external join",
			channel: drivers.VectorTestChannel(true, true),
			config:  drivers.Config{ID: 1, Name: "vector", External: true},
		},
		{
			name:    "NI-XNET classic with termination off",
			channel: drivers.NIXNETTestChannel(true, true),
			config:  drivers.Config{ID: 1, Name: "ni", Bitrate: 500_000, Termination: drivers.TerminationOff},
		},
		{
			name:    "NI-XNET classic with termination on",
			channel: drivers.NIXNETTestChannel(true, true),
			config:  drivers.Config{ID: 1, Name: "ni", Bitrate: 500_000, Termination: drivers.TerminationOn},
		},
		{
			name:    "NI-XNET equivalent 80 MHz FD timing",
			channel: drivers.NIXNETTestChannel(true, true),
			config: drivers.Config{
				ID: 1, Name: "ni",
				FDTiming: drivers.FDTiming{
					ClockHz: 80_000_000,
					Nominal: drivers.BitTiming{BRP: 2, TSEG1: 63, TSEG2: 16, SJW: 16},
					Data:    drivers.BitTiming{BRP: 2, TSEG1: 15, TSEG2: 4, SJW: 4},
				},
			},
		},
		{
			name:    "ZLG classic USBCAN",
			channel: drivers.ZLGTestChannel(false),
			config:  drivers.Config{ID: 1, Name: "zlg", Bitrate: 500_000},
		},
		{
			name:    "ZLG classic on USBCANFD with termination",
			channel: drivers.ZLGTestChannel(true),
			config:  drivers.Config{ID: 1, Name: "zlg", Bitrate: 500_000, Termination: drivers.TerminationOn},
		},
		{
			name:    "SocketCAN external",
			channel: drivers.SocketCANTestChannel(true),
			config:  drivers.Config{ID: 1, Name: "can0", External: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bus, err := drivers.Open(ctx, capture, test.channel, test.config)
			if bus != nil {
				_ = bus.Close()
				t.Fatal("Open returned a bus with a canceled context")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Open error = %v; want %v (valid configuration)", err, context.Canceled)
			}
		})
	}
}

func TestOpenRejectsInvalidConfigurations(t *testing.T) {
	ctx := canceledContext()
	capture := gocan.NewCapture()
	pcanFD := drivers.PCANTestChannel(true)
	pcanClassic := drivers.PCANTestChannel(false)
	vectorFD := drivers.VectorTestChannel(true, false)
	vectorExternal := drivers.VectorTestChannel(true, true)
	nixnetFD := drivers.NIXNETTestChannel(true, true)
	nixnetNoTerm := drivers.NIXNETTestChannel(true, false)
	zlgFD := drivers.ZLGTestChannel(true)
	zlgClassic := drivers.ZLGTestChannel(false)
	socketcanFD := drivers.SocketCANTestChannel(true)

	tests := []struct {
		name    string
		capture *gocan.Capture
		channel drivers.Channel
		config  drivers.Config
	}{
		// Missing required arguments.
		{"nil capture", nil, pcanFD, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000}},
		{"undiscovered channel", capture, drivers.Channel{}, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000}},
		{"missing ID", capture, pcanFD, drivers.Config{Name: "can", Bitrate: 500_000}},
		{"missing name", capture, pcanFD, drivers.Config{ID: 1, Bitrate: 500_000}},

		// Timing mode exclusivity.
		{"no timing mode", capture, pcanFD, drivers.Config{ID: 1, Name: "can"}},
		{"bitrate and external", capture, pcanFD, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000, External: true}},
		{"bitrate and FDTiming", capture, pcanFD, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000, FDTiming: qualifiedFDTiming}},
		{"FDTiming and external", capture, pcanFD, drivers.Config{ID: 1, Name: "can", FDTiming: qualifiedFDTiming, External: true}},
		{"preset and external", capture, pcanFD, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000, DataBitrate: 2_000_000, External: true}},
		{"preset and FDTiming", capture, pcanFD, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000, DataBitrate: 2_000_000, FDTiming: qualifiedFDTiming}},
		{"data bitrate and FDTiming", capture, pcanFD, drivers.Config{ID: 1, Name: "can", DataBitrate: 2_000_000, FDTiming: qualifiedFDTiming}},
		{"data bitrate and external", capture, pcanFD, drivers.Config{ID: 1, Name: "can", DataBitrate: 2_000_000, External: true}},
		{"data bitrate without nominal", capture, pcanFD, drivers.Config{ID: 1, Name: "can", DataBitrate: 2_000_000}},

		// Timing ownership mismatches.
		{"programmable channel with external", capture, pcanFD, drivers.Config{ID: 1, Name: "can", External: true}},
		{"SocketCAN with bitrate", capture, socketcanFD, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000}},
		{"SocketCAN with FD preset", capture, socketcanFD, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000, DataBitrate: 2_000_000}},
		{"shared Vector with bitrate", capture, vectorExternal, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000}},

		// Capability mismatches.
		{"classic PCAN with FD preset", capture, pcanClassic, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000, DataBitrate: 2_000_000}},
		{"classic PCAN with FDTiming", capture, pcanClassic, drivers.Config{ID: 1, Name: "can", FDTiming: qualifiedFDTiming}},
		{"classic ZLG with FD preset", capture, zlgClassic, drivers.Config{ID: 1, Name: "can", Bitrate: 500_000, DataBitrate: 2_000_000}},

		// Termination validation.
		{"unknown termination value", capture, nixnetFD, drivers.Config{ID: 1, Name: "ni", Bitrate: 500_000, Termination: drivers.Termination(3)}},
		{"termination off unsupported", capture, nixnetNoTerm, drivers.Config{ID: 1, Name: "ni", Bitrate: 500_000, Termination: drivers.TerminationOff}},
		{"termination on unsupported", capture, pcanFD, drivers.Config{ID: 1, Name: "pcan", Bitrate: 500_000, Termination: drivers.TerminationOn}},
		{"termination on classic ZLG", capture, zlgClassic, drivers.Config{ID: 1, Name: "zlg", Bitrate: 500_000, Termination: drivers.TerminationOn}},

		// Generic FDTiming bounds. Each case isolates a single rule.
		{"FDTiming zero clock", capture, pcanFD, drivers.Config{ID: 1, Name: "can", FDTiming: drivers.FDTiming{Nominal: qualifiedFDTiming.Nominal, Data: qualifiedFDTiming.Data}}},
		{"FDTiming zero BRP", capture, pcanFD, drivers.Config{ID: 1, Name: "can", FDTiming: drivers.FDTiming{
			ClockHz: 80_000_000,
			Nominal: drivers.BitTiming{BRP: 0, TSEG1: 119, TSEG2: 40, SJW: 1},
			Data:    qualifiedFDTiming.Data,
		}}},
		{"FDTiming zero TSEG1", capture, pcanFD, drivers.Config{ID: 1, Name: "can", FDTiming: drivers.FDTiming{
			ClockHz: 80_000_000,
			Nominal: drivers.BitTiming{BRP: 1, TSEG1: 0, TSEG2: 39, SJW: 1},
			Data:    qualifiedFDTiming.Data,
		}}},
		{"FDTiming zero TSEG2", capture, pcanFD, drivers.Config{ID: 1, Name: "can", FDTiming: drivers.FDTiming{
			ClockHz: 80_000_000,
			Nominal: drivers.BitTiming{BRP: 1, TSEG1: 159, TSEG2: 0, SJW: 1},
			Data:    qualifiedFDTiming.Data,
		}}},
		{"FDTiming zero SJW", capture, pcanFD, drivers.Config{ID: 1, Name: "can", FDTiming: drivers.FDTiming{
			ClockHz: 80_000_000,
			Nominal: drivers.BitTiming{BRP: 1, TSEG1: 119, TSEG2: 40, SJW: 0},
			Data:    qualifiedFDTiming.Data,
		}}},
		// 1 + 158 + 1 = 160 quanta divides 80 MHz cleanly, isolating SJW > TSEG2.
		{"FDTiming SJW exceeds TSEG2", capture, pcanFD, drivers.Config{ID: 1, Name: "can", FDTiming: drivers.FDTiming{
			ClockHz: 80_000_000,
			Nominal: drivers.BitTiming{BRP: 1, TSEG1: 158, TSEG2: 1, SJW: 2},
			Data:    qualifiedFDTiming.Data,
		}}},
		{"FDTiming nonintegral bitrate", capture, pcanFD, drivers.Config{ID: 1, Name: "can", FDTiming: drivers.FDTiming{
			ClockHz: 80_000_001,
			Nominal: qualifiedFDTiming.Nominal,
			Data:    qualifiedFDTiming.Data,
		}}},
		// (2^32 - 1) * (2^32 + 2) wraps mod 2^64 to 0xfffffffe, which divides ClockHz
		// evenly, so only the overflow guard rejects this input.
		{"FDTiming denominator overflow", capture, pcanFD, drivers.Config{ID: 1, Name: "can", FDTiming: drivers.FDTiming{
			ClockHz: 0xfffffffe,
			Nominal: drivers.BitTiming{BRP: math.MaxUint32, TSEG1: 1<<31 + 1, TSEG2: 1 << 31, SJW: 1},
			Data:    drivers.BitTiming{BRP: 1, TSEG1: 1, TSEG2: 0xfffffffe - 2, SJW: 1},
		}}},

		// Driver-specific bitrate and timing rules.
		{"PCAN unsupported classical bitrate", capture, pcanFD, drivers.Config{ID: 1, Name: "pcan", Bitrate: 300_000}},
		{"PCAN unsupported FD nominal preset", capture, pcanFD, drivers.Config{ID: 1, Name: "pcan", Bitrate: 250_000, DataBitrate: 2_000_000}},
		{"Vector unsupported FD data preset", capture, vectorFD, drivers.Config{ID: 1, Name: "vector", Bitrate: 500_000, DataBitrate: 3_000_000}},
		{"Vector non-80MHz FD clock", capture, vectorFD, drivers.Config{ID: 1, Name: "vector", FDTiming: drivers.FDTiming{
			ClockHz: 40_000_000,
			Nominal: drivers.BitTiming{BRP: 1, TSEG1: 63, TSEG2: 16, SJW: 16},
			Data:    drivers.BitTiming{BRP: 1, TSEG1: 15, TSEG2: 4, SJW: 4},
		}}},
		{"NI-XNET non-25ns FD quantum", capture, nixnetFD, drivers.Config{ID: 1, Name: "ni", FDTiming: qualifiedFDTiming}},
		// 1 + 257 + 62 = 320 quanta divides 40 MHz cleanly, isolating nominal TSEG1 > 256.
		{"NI-XNET nominal TSEG1 exceeds limit", capture, nixnetFD, drivers.Config{ID: 1, Name: "ni", FDTiming: drivers.FDTiming{
			ClockHz: 40_000_000,
			Nominal: drivers.BitTiming{BRP: 1, TSEG1: 257, TSEG2: 62, SJW: 1},
			Data:    drivers.BitTiming{BRP: 1, TSEG1: 15, TSEG2: 4, SJW: 4},
		}}},
		// 1 + 33 + 6 = 40 quanta divides 40 MHz cleanly, isolating data TSEG1 > 32.
		{"NI-XNET data TSEG1 exceeds limit", capture, nixnetFD, drivers.Config{ID: 1, Name: "ni", FDTiming: drivers.FDTiming{
			ClockHz: 40_000_000,
			Nominal: drivers.BitTiming{BRP: 1, TSEG1: 63, TSEG2: 16, SJW: 16},
			Data:    drivers.BitTiming{BRP: 1, TSEG1: 33, TSEG2: 6, SJW: 1},
		}}},
		{"ZLG custom FDTiming rejected", capture, zlgFD, drivers.Config{ID: 1, Name: "zlg", FDTiming: qualifiedFDTiming}},
		{"ZLG unsupported FD preset", capture, zlgFD, drivers.Config{ID: 1, Name: "zlg", Bitrate: 500_000, DataBitrate: 3_000_000}},
		{"ZLG unsupported classical bitrate", capture, zlgClassic, drivers.Config{ID: 1, Name: "zlg", Bitrate: 300_000}},
		{"ZLG USBCANFD unsupported classical bitrate", capture, zlgFD, drivers.Config{ID: 1, Name: "zlg", Bitrate: 300_000}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bus, err := drivers.Open(ctx, test.capture, test.channel, test.config)
			if bus != nil {
				_ = bus.Close()
				t.Fatal("Open unexpectedly returned a bus")
			}
			if !errors.Is(err, drivers.ErrInvalidConfig) {
				t.Fatalf("Open error = %v; want %v", err, drivers.ErrInvalidConfig)
			}
		})
	}
}

func TestFDRatePresetsContract(t *testing.T) {
	ctx := canceledContext()
	capture := gocan.NewCapture()

	fdChannels := []drivers.Channel{
		drivers.PCANTestChannel(true),
		drivers.VectorTestChannel(true, false),
		drivers.NIXNETTestChannel(true, true),
		drivers.ZLGTestChannel(true),
	}
	for _, channel := range fdChannels {
		t.Run(channel.Driver(), func(t *testing.T) {
			presets := channel.FDRatePresets()
			if len(presets) == 0 {
				t.Fatalf("%s returned no FD rate presets", channel.Driver())
			}

			// Mutating the returned slice must not alter driver state.
			first := presets[0]
			presets[0] = drivers.FDRatePreset{Bitrate: 1, DataBitrate: 2}
			again := channel.FDRatePresets()
			if len(again) != len(presets) || again[0] != first {
				t.Fatalf("consumer mutation leaked into %s presets: got %v, want first=%v", channel.Driver(), again, first)
			}

			// Every advertised preset must pass Open validation.
			for _, preset := range again {
				_, err := drivers.Open(ctx, capture, channel, drivers.Config{
					ID:          3,
					Name:        "powertrain",
					Bitrate:     preset.Bitrate,
					DataBitrate: preset.DataBitrate,
				})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Open(%s, preset %+v) = %v; want %v", channel.Driver(), preset, err, context.Canceled)
				}
			}
		})
	}

	unavailable := []drivers.Channel{
		{},
		drivers.PCANTestChannel(false),
		drivers.VectorTestChannel(false, false),
		drivers.VectorTestChannel(true, true),
		drivers.NIXNETTestChannel(false, false),
		drivers.ZLGTestChannel(false),
		drivers.SocketCANTestChannel(true),
	}
	for _, channel := range unavailable {
		if presets := channel.FDRatePresets(); len(presets) != 0 {
			t.Errorf("channel %s lists unavailable presets: %v", channel, presets)
		}
	}
}

type foreignVendorOptions struct {
	drivers.VendorOptions
}

type wrappedNIXNETOptions struct {
	drivers.NIXNETOptions
}

func TestVendorOptions(t *testing.T) {
	ctx := canceledContext()
	capture := gocan.NewCapture()

	nixnetCh := drivers.NIXNETTestChannel(true, true)
	for _, tc := range []struct {
		name   string
		vendor drivers.VendorOptions
	}{
		{"nil", nil},
		{"value false", drivers.NIXNETOptions{ConcurrentIO: false}},
		{"value true", drivers.NIXNETOptions{ConcurrentIO: true}},
		{"pointer true", &drivers.NIXNETOptions{ConcurrentIO: true}},
	} {
		t.Run("nixnet accepts "+tc.name, func(t *testing.T) {
			_, err := drivers.Open(ctx, capture, nixnetCh, drivers.Config{
				ID:      1,
				Name:    "ni",
				Bitrate: 500_000,
				Vendor:  tc.vendor,
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Open(nixnet, %s) = %v; want %v", tc.name, err, context.Canceled)
			}
		})
	}

	// Typed nil and foreign/wrapped option types must be rejected on NI-XNET too.
	for _, tc := range []struct {
		name   string
		vendor drivers.VendorOptions
	}{
		{"typed nil pointer", (*drivers.NIXNETOptions)(nil)},
		{"embedded interface", foreignVendorOptions{}},
		{"embedded NIXNETOptions", wrappedNIXNETOptions{}},
	} {
		t.Run("nixnet rejects "+tc.name, func(t *testing.T) {
			_, err := drivers.Open(ctx, capture, nixnetCh, drivers.Config{
				ID:      1,
				Name:    "ni",
				Bitrate: 500_000,
				Vendor:  tc.vendor,
			})
			if !errors.Is(err, drivers.ErrInvalidConfig) {
				t.Fatalf("Open(nixnet, %s) = %v; want %v", tc.name, err, drivers.ErrInvalidConfig)
			}
		})
	}

	// Non-NI-XNET drivers must reject mismatched or unknown vendor options.
	rejectedByOtherDrivers := []struct {
		name    string
		channel drivers.Channel
		config  drivers.Config
	}{
		{"pcan mismatched zero", drivers.PCANTestChannel(true), drivers.Config{ID: 1, Name: "pcan", Bitrate: 500_000, Vendor: drivers.NIXNETOptions{}}},
		{"pcan foreign", drivers.PCANTestChannel(true), drivers.Config{ID: 1, Name: "pcan", Bitrate: 500_000, Vendor: foreignVendorOptions{}}},
		{"vector mismatched", drivers.VectorTestChannel(true, false), drivers.Config{ID: 1, Name: "vector", Bitrate: 500_000, Vendor: drivers.NIXNETOptions{ConcurrentIO: true}}},
		{"vector external mismatched", drivers.VectorTestChannel(true, true), drivers.Config{ID: 1, Name: "vector", External: true, Vendor: drivers.NIXNETOptions{ConcurrentIO: true}}},
		{"zlg mismatched", drivers.ZLGTestChannel(true), drivers.Config{ID: 1, Name: "zlg", Bitrate: 500_000, Vendor: drivers.NIXNETOptions{ConcurrentIO: true}}},
		{"socketcan mismatched", drivers.SocketCANTestChannel(true), drivers.Config{ID: 1, Name: "can0", External: true, Vendor: drivers.NIXNETOptions{ConcurrentIO: true}}},
	}
	for _, tc := range rejectedByOtherDrivers {
		t.Run(tc.name, func(t *testing.T) {
			_, err := drivers.Open(ctx, capture, tc.channel, tc.config)
			if !errors.Is(err, drivers.ErrInvalidConfig) {
				t.Fatalf("Open(%s) = %v; want %v", tc.name, err, drivers.ErrInvalidConfig)
			}
		})
	}
}
