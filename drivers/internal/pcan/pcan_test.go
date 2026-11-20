package pcan

import (
	"math"
	"testing"
	"unsafe"

	"github.com/tomrford/gocan"
)

// TestClassicalBitrateTable checks every predefined code against the SJA1000
// timing it encodes: the bit rate is 8 MHz / ((BRP+1) * (3 + TSEG1 + TSEG2)),
// and the table key is that rate rounded to the nearest bit per second.
func TestClassicalBitrateTable(t *testing.T) {
	for bitsPerSecond, code := range classicalBitrates {
		brp := (uint32(code>>8) & 0x3f) + 1
		tseg1 := uint32(code) & 0x0f
		tseg2 := uint32(code>>4) & 0x07
		encoded := 8_000_000 / float64(brp*(3+tseg1+tseg2))
		if got := uint32(math.Round(encoded)); got != bitsPerSecond {
			t.Errorf("BTR0BTR1 %#04x encodes %d bit/s but is keyed as %d", code, got, bitsPerSecond)
		}
	}
}

func TestNativeLayouts(t *testing.T) {
	if got := unsafe.Sizeof(pcanMsg{}); got != 16 {
		t.Errorf("TPCANMsg size = %d, want 16", got)
	}
	if got := unsafe.Offsetof(pcanMsg{}.data); got != 6 {
		t.Errorf("TPCANMsg DATA offset = %d, want 6", got)
	}
	if got := unsafe.Sizeof(pcanMsgFD{}); got != 72 {
		t.Errorf("TPCANMsgFD size = %d, want 72", got)
	}
	if got := unsafe.Offsetof(pcanMsgFD{}.data); got != 6 {
		t.Errorf("TPCANMsgFD DATA offset = %d, want 6", got)
	}
	if got := unsafe.Sizeof(pcanChannelInformation{}); got != 52 {
		t.Errorf("TPCANChannelInformation size = %d, want 52", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.deviceType); got != 2 {
		t.Errorf("TPCANChannelInformation device type offset = %d, want 2", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.controllerNumber); got != 3 {
		t.Errorf("TPCANChannelInformation controller number offset = %d, want 3", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.deviceFeatures); got != 4 {
		t.Errorf("TPCANChannelInformation device features offset = %d, want 4", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.deviceName); got != 8 {
		t.Errorf("TPCANChannelInformation device name offset = %d, want 8", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.deviceID); got != 44 {
		t.Errorf("TPCANChannelInformation device ID offset = %d, want 44", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.channelCondition); got != 48 {
		t.Errorf("TPCANChannelInformation channel condition offset = %d, want 48", got)
	}
}

func TestPCANEventTranslation(t *testing.T) {
	// Receive ESI comes from the controller, so normal hardware sends cannot
	// guarantee exercising this native flag.
	fd, err := decodePCANReceive(0x123, 0x1c, 10, []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, true, pcanStatusOK, 1)
	want := gocan.Frame{ID: 0x123, DLC: 10, Flags: gocan.FrameFD | gocan.FrameBitRateSwitch | gocan.FrameErrorStateIndicator}
	copy(want.Data[:], []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})
	if err != nil || !fd.hasFrame || fd.frame != want {
		t.Fatalf("FD receive flags = %+v, %v; want %+v", fd, err, want)
	}

	errorFrame, err := decodePCANReceive(
		0x08,
		pcanMessageError,
		4,
		[]byte{0x00, 0x19, 0x00, 0x80},
		false,
		pcanStatusOK,
		1,
	)
	if err != nil || errorFrame.eventCount != 2 ||
		errorFrame.events[0].Kind != gocan.EventErrorFrame {
		t.Fatalf("error frame = %+v, %v", errorFrame, err)
	}
	state := errorFrame.events[1]
	if state.Kind != gocan.EventControllerState ||
		state.ControllerState != gocan.ControllerPassive ||
		state.TXErrorCount != 128 || state.RXErrorCount != 0 ||
		!state.ErrorCountsKnown {
		t.Fatalf("error-frame state = %+v", errorFrame)
	}

	// Bench PCAN-USB status payloads at bus-off: 0x14 and 0x18 (November 2026).
	for _, payload := range []byte{0x10, 0x14, 0x18} {
		busOff, err := decodePCANReceive(
			1,
			pcanMessageStatus,
			4,
			[]byte{0x00, 0x00, 0x00, payload},
			false,
			pcanStatus(payload),
			1,
		)
		if err != nil || busOff.eventCount != 1 ||
			busOff.events[0].ControllerState != gocan.ControllerBusOff {
			t.Fatalf("bus-off status %#x = %+v, %v", payload, busOff, err)
		}
	}

	for _, status := range []pcanStatus{pcanStatusOverrun, pcanStatusQueueOverrun} {
		overrun, err := decodePCANStatus(status, 1)
		if err != nil || overrun.eventCount != 1 ||
			overrun.events[0].Kind != gocan.EventReceiveOverrun {
			t.Fatalf("overrun status %#x = %+v, %v", status, overrun, err)
		}
	}

	combined, err := decodePCANStatus(pcanStatusOverrun|pcanStatusBusOff, 1)
	if err != nil || combined.eventCount != 2 ||
		combined.events[0].Kind != gocan.EventReceiveOverrun ||
		combined.events[1].ControllerState != gocan.ControllerBusOff {
		t.Fatalf("overrun with bus-off = %+v, %v", combined, err)
	}
}
