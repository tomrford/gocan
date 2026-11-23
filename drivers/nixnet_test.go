package drivers

import (
	"testing"

	"github.com/tomrford/gocan/drivers/internal/drivercfg"
)

func TestNIXNETNativeTranslation(t *testing.T) {
	channel := NIXNETTestChannel(true, true)
	if channel.Identifier() != "nixnet:CAN1" || channel.Driver() != "nixnet" || !channel.SupportsTermination() {
		t.Fatalf("unexpected channel metadata: %+v", channel)
	}

	// Verify Vendor option, Termination, and 500k/2M bitfield encoding.
	got, err := nativeNIXNETConfig(channel, preparedConfig{
		config: Config{
			ID:          1,
			Name:        "NI",
			Bitrate:     500_000,
			DataBitrate: 2_000_000,
			Vendor:      NIXNETOptions{ConcurrentIO: true},
		},
		termination: drivercfg.TerminationOn,
		fd:          true,
	})
	if err != nil {
		t.Fatalf("nativeNIXNETConfig 500k/2M: %v", err)
	}
	if !got.FD || !got.ConcurrentIO || got.Termination != drivercfg.TerminationOn {
		t.Fatalf("unexpected native flags: %+v", got)
	}
	if got.Baud != 0x19a00f3e0f || got.FDBaud != 0x801000a0032e33 {
		t.Fatalf("encoded 500k/2M timing = %x/%x; want 19a00f3e0f/801000a0032e33", got.Baud, got.FDBaud)
	}

	// Verify 500k/4M TDCO bitfield encoding.
	got4M, err := nativeNIXNETConfig(channel, preparedConfig{
		config: Config{
			ID:          1,
			Name:        "NI",
			Bitrate:     500_000,
			DataBitrate: 4_000_000,
		},
		termination: drivercfg.TerminationOff,
		fd:          true,
	})
	if err != nil {
		t.Fatalf("nativeNIXNETConfig 500k/4M: %v", err)
	}
	if got4M.ConcurrentIO || got4M.Termination != drivercfg.TerminationOff {
		t.Fatalf("unexpected native 4M flags: %+v", got4M)
	}
	if got4M.Baud != 0x19a00f3e0f || got4M.FDBaud != 0x800800a0032611 {
		t.Fatalf("encoded 500k/4M timing = %x/%x; want 19a00f3e0f/800800a0032611", got4M.Baud, got4M.FDBaud)
	}

	// Verify equivalent 80 MHz clock translates identically to 40 MHz quanta.
	gotEquiv, err := nativeNIXNETConfig(channel, preparedConfig{
		config: Config{
			ID:   1,
			Name: "NI",
			FDTiming: FDTiming{
				ClockHz: 80_000_000,
				Nominal: BitTiming{BRP: 2, TSEG1: 63, TSEG2: 16, SJW: 16},
				Data:    BitTiming{BRP: 2, TSEG1: 15, TSEG2: 4, SJW: 4},
			},
		},
		fd: true,
	})
	if err != nil {
		t.Fatalf("nativeNIXNETConfig equivalent clock: %v", err)
	}
	if gotEquiv.Baud != 0x19a00f3e0f || gotEquiv.FDBaud != 0x801000a0032e33 {
		t.Fatalf("equivalent clock translation = %x/%x; want 19a00f3e0f/801000a0032e33", gotEquiv.Baud, gotEquiv.FDBaud)
	}
}
