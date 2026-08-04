package pcan

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
)

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
