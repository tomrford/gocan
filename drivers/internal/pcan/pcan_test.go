package pcan

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
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

func TestFrameTranslationRoundTripsNativeForms(t *testing.T) {
	remote, err := gocan.NewRemoteFrame(gocan.MaxExtendedID, 8, true)
	if err != nil {
		t.Fatalf("NewRemoteFrame: %v", err)
	}
	fd, err := gocan.NewFrame(
		0x123,
		[]byte{
			0, 1, 2, 3, 4, 5, 6, 7,
			8, 9, 10, 11, 12, 13, 14, 15,
		},
		gocan.FrameFD|gocan.FrameBitRateSwitch|gocan.FrameErrorStateIndicator,
	)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}

	nativeRemote := encodeClassicMessage(remote)
	if nativeRemote.id != gocan.MaxExtendedID || nativeRemote.messageType != 0x03 || nativeRemote.length != 8 {
		t.Errorf("encoded classic remote = %+v, want ID %#x, type 0x03, length 8", nativeRemote, gocan.MaxExtendedID)
	}
	gotRemote, err := decodeClassicMessage(nativeRemote)
	if err != nil {
		t.Fatalf("decode classic remote: %v", err)
	}
	if gotRemote != remote {
		t.Errorf("classic remote round trip = %+v, want %+v", gotRemote, remote)
	}

	nativeFD := encodeFDMessage(fd)
	if nativeFD.id != 0x123 || nativeFD.messageType != 0x1c || nativeFD.dlc != 10 {
		t.Errorf("encoded FD frame = %+v, want ID 0x123, type 0x1c, DLC 10", nativeFD)
	}
	gotFD, err := decodeFDMessage(nativeFD)
	if err != nil {
		t.Fatalf("decode FD: %v", err)
	}
	if gotFD != fd {
		t.Errorf("FD round trip = %+v, want %+v", gotFD, fd)
	}

	classicalOnFD, err := decodeFDMessage(encodeFDMessage(remote))
	if err != nil {
		t.Fatalf("decode classical frame from FD API: %v", err)
	}
	if classicalOnFD != remote {
		t.Errorf("classical FD-API round trip = %+v, want %+v", classicalOnFD, remote)
	}
}

func TestClassicalDLCAboveEightOnFDAPI(t *testing.T) {
	for _, dlc := range []uint8{9, 12, 15} {
		for _, remote := range []bool{false, true} {
			name := fmt.Sprintf("DLC_%d/data", dlc)
			flags := gocan.FrameFlags(0)
			if remote {
				name = fmt.Sprintf("DLC_%d/RTR", dlc)
				flags = gocan.FrameRemote
			}
			t.Run(name, func(t *testing.T) {
				frame := gocan.Frame{ID: 0x321, DLC: dlc, Flags: flags}
				if !remote {
					copy(frame.Data[:8], []byte{0, 1, 2, 3, 4, 5, 6, 7})
				}
				if err := frame.Validate(); err != nil {
					t.Fatalf("validate fixture: %v", err)
				}

				if err := validateSendFrame(frame, false); err == nil ||
					!strings.Contains(err.Error(), fmt.Sprintf("cannot send DLC %d", dlc)) {
					t.Fatalf("classic API validation = %v, want DLC rejection", err)
				}
				if err := validateSendFrame(frame, true); err != nil {
					t.Fatalf("FD API validation: %v", err)
				}

				native := encodeFDMessage(frame)
				if native.dlc != dlc {
					t.Fatalf("encoded DLC = %d, want %d", native.dlc, dlc)
				}
				if native.messageType&pcanMessageFD != 0 {
					t.Fatalf("encoded classical message type %#02x has FD flag", native.messageType)
				}
				wantData := [8]byte{}
				if !remote {
					wantData = [8]byte{0, 1, 2, 3, 4, 5, 6, 7}
				}
				if got := [8]byte(native.data[:8]); got != wantData {
					t.Fatalf("encoded data = %v, want %v", got, wantData)
				}
				if !remote {
					native.data[8] = 0xff
				}

				got, err := decodeFDMessage(native)
				if err != nil {
					t.Fatalf("decode classical frame through FD API: %v", err)
				}
				if got != frame {
					t.Fatalf("FD API round trip = %+v, want %+v", got, frame)
				}
			})
		}
	}
}

func TestFrameTranslationRejectsUnrepresentedNativeMessages(t *testing.T) {
	_, err := decodeFDMessage(pcanMsgFD{
		id:          0x123,
		messageType: pcanMessageExtended | pcanMessageStatus,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported PCAN message type") {
		t.Fatalf("decode status message error = %v, want explicit unsupported error", err)
	}
}

func TestStatusFrameUsesNetworkByteOrder(t *testing.T) {
	status, ok := decodeStatusFrame(pcanMessageStatus, []byte{0x00, 0x00, 0x00, 0x40})
	if !ok || status != pcanStatusQueueOverrun {
		t.Fatalf("decoded status = %#x, %t; want %#x, true", status, ok, pcanStatusQueueOverrun)
	}
}

func TestPCANEventTranslation(t *testing.T) {
	timestamp := time.Unix(1, 2)
	errorFrame, err := decodePCANReceive(
		0x08,
		pcanMessageError,
		4,
		[]byte{0x00, 0x19, 0x00, 0x80},
		false,
		pcanStatusOK,
		1,
		timestamp,
	)
	if err != nil || errorFrame.eventCount != 2 ||
		errorFrame.events[0].Kind != gocan.EventErrorFrame {
		t.Fatalf("error frame = %+v, %v", errorFrame, err)
	}
	state := errorFrame.events[1]
	if state.Kind != gocan.EventControllerState ||
		state.ControllerState != gocan.ControllerPassive ||
		state.TXErrorCount != 128 || state.RXErrorCount != 0 ||
		!state.ErrorCountsKnown || errorFrame.terminal != nil {
		t.Fatalf("error-frame state = %+v", errorFrame)
	}

	busOff, err := decodePCANReceive(
		0,
		pcanMessageStatus,
		4,
		[]byte{0x00, 0x00, 0x00, 0x10},
		false,
		pcanStatusBusOff,
		1,
		timestamp,
	)
	if err != nil || !errors.Is(busOff.terminal, gocan.ErrBusOff) ||
		busOff.events[0].ControllerState != gocan.ControllerBusOff {
		t.Fatalf("bus-off status = %+v, %v", busOff, err)
	}

	overrun, err := decodePCANStatus(pcanStatusQueueOverrun, 1, timestamp)
	if err != nil || !errors.Is(overrun.terminal, gocan.ErrReceiveOverrun) ||
		overrun.events[0].Kind != gocan.EventReceiveOverrun {
		t.Fatalf("overrun status = %+v, %v", overrun, err)
	}
}
