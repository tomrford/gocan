package vector

import (
	"errors"
	"fmt"
	"testing"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
)

func TestClassicNativeLayoutAndTranslation(t *testing.T) {
	if got := unsafe.Sizeof(xlCanMessage{}); got != 32 {
		t.Fatalf("s_xl_can_msg size = %d, want 32", got)
	}
	if got := unsafe.Sizeof(xlEvent{}); got != 48 {
		t.Fatalf("XLevent size = %d, want 48", got)
	}
	if got := unsafe.Offsetof(xlEvent{}.timestamp); got != 8 {
		t.Fatalf("XLevent timestamp offset = %d, want 8", got)
	}
	if got := unsafe.Offsetof(xlEvent{}.tagData); got != 16 {
		t.Fatalf("XLevent tagData offset = %d, want 16", got)
	}

	frame, err := gocan.NewFrame(gocan.MaxExtendedID, []byte{1, 2, 3}, gocan.FrameExtended)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	event := encodeEvent(frame)
	event.tag = xlEventReceiveMessage
	timestamp := time.Unix(1, 2)
	got, err := decodeClassicReceiveEvent(&event, 1, timestamp)
	if err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if !got.hasFrame || got.frame != frame {
		t.Fatalf("round trip = %+v, want frame %+v", got, frame)
	}

	event.flags = xlEventFlagOverrun
	overrun, err := decodeClassicReceiveEvent(&event, 1, timestamp)
	if err != nil || !errors.Is(overrun.terminal, gocan.ErrReceiveOverrun) ||
		overrun.eventCount != 1 || overrun.events[0].Kind != gocan.EventReceiveOverrun {
		t.Fatalf("decode overrun = %+v, %v", overrun, err)
	}
}

func TestFDNativeLayoutAndTranslation(t *testing.T) {
	if got := unsafe.Sizeof(xlCANFDConfig{}); got != 40 {
		t.Fatalf("XLcanFdConf size = %d, want 40", got)
	}
	if got := unsafe.Sizeof(xlCANRXMessage{}); got != 96 {
		t.Fatalf("s_xl_can_ev_rx_msg size = %d, want 96", got)
	}
	if got := unsafe.Sizeof(xlCANRXEvent{}); got != 128 {
		t.Fatalf("XLcanRxEvent size = %d, want 128", got)
	}
	if got := unsafe.Offsetof(xlCANRXEvent{}.timestamp); got != 24 {
		t.Fatalf("XLcanRxEvent timestamp offset = %d, want 24", got)
	}
	if got := unsafe.Offsetof(xlCANRXEvent{}.tagData); got != 32 {
		t.Fatalf("XLcanRxEvent tagData offset = %d, want 32", got)
	}
	if got := unsafe.Sizeof(xlCANTXEvent{}); got != 88 {
		t.Fatalf("XLcanTxEvent size = %d, want 88", got)
	}
	if got := unsafe.Offsetof(xlCANTXEvent{}.message); got != 8 {
		t.Fatalf("XLcanTxEvent tagData offset = %d, want 8", got)
	}

	data := make([]byte, 64)
	for index := range data {
		data[index] = byte(index)
	}
	frame, err := gocan.NewFrame(
		gocan.MaxExtendedID,
		data,
		gocan.FrameExtended|gocan.FrameFD|gocan.FrameBitRateSwitch,
	)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	tx := encodeFDTransmitEvent(frame)
	if tx.tag != xlCANEventTX || tx.transactionID != 0xffff {
		t.Fatalf("FD transmit header = tag %d transaction %#x", tx.tag, tx.transactionID)
	}

	rx := xlCANRXEvent{tag: xlCANEventRXOK}
	rxMessage := rx.message()
	rxMessage.id = tx.message.id
	rxMessage.flags = tx.message.flags | xlCANMessageFlagESI
	rxMessage.dlc = tx.message.dlc
	rxMessage.data = tx.message.data
	got, err := decodeFDReceiveEvent(&rx, 1, time.Unix(1, 2))
	if err != nil {
		t.Fatalf("decode FD event: %v", err)
	}
	frame.Flags |= gocan.FrameErrorStateIndicator
	if !got.hasFrame || got.frame != frame {
		t.Fatalf("FD round trip = %+v, want frame %+v", got, frame)
	}
}

func TestClassicalDLCAboveEightNativeTranslation(t *testing.T) {
	for _, nativeAPI := range []string{"classic", "FD"} {
		for dlc := uint8(9); dlc <= 15; dlc++ {
			for _, remote := range []bool{false, true} {
				kind := "data"
				flags := gocan.FrameFlags(0)
				if remote {
					kind = "RTR"
					flags = gocan.FrameRemote
				}
				t.Run(fmt.Sprintf("%s/DLC_%d/%s", nativeAPI, dlc, kind), func(t *testing.T) {
					frame := gocan.Frame{ID: 0x321, DLC: dlc, Flags: flags}
					if !remote {
						copy(frame.Data[:8], []byte{0, 1, 2, 3, 4, 5, 6, 7})
					}
					if err := frame.Validate(); err != nil {
						t.Fatalf("validate fixture: %v", err)
					}

					var (
						got receiveObservation
						err error
					)
					switch nativeAPI {
					case "classic":
						event := encodeEvent(frame)
						event.tag = xlEventReceiveMessage
						if event.message().dlc != uint16(dlc) {
							t.Fatalf("encoded DLC = %d, want %d", event.message().dlc, dlc)
						}
						got, err = decodeClassicReceiveEvent(&event, 1, time.Unix(1, 2))
					case "FD":
						tx := encodeFDTransmitEvent(frame)
						if tx.message.dlc != dlc {
							t.Fatalf("encoded DLC = %d, want %d", tx.message.dlc, dlc)
						}
						if tx.message.flags&xlCANMessageFlagEDL != 0 {
							t.Fatalf("encoded classical flags %#x have EDL set", tx.message.flags)
						}
						if !remote {
							tx.message.data[8] = 0xff
						}
						rx := xlCANRXEvent{tag: xlCANEventRXOK}
						*rx.message() = xlCANRXMessage{
							id:    tx.message.id,
							flags: tx.message.flags,
							dlc:   tx.message.dlc,
							data:  tx.message.data,
						}
						got, err = decodeFDReceiveEvent(&rx, 1, time.Unix(1, 2))
					default:
						t.Fatalf("unsupported native API %q", nativeAPI)
					}
					if err != nil {
						t.Fatalf("decode: %v", err)
					}
					if !got.hasFrame || got.frame != frame {
						t.Fatalf("round trip = %+v, want frame %+v", got, frame)
					}
				})
			}
		}
	}
}

func TestVectorStatusTranslation(t *testing.T) {
	timestamp := time.Unix(1, 2)
	event := xlEvent{tag: xlEventReceiveMessage}
	event.message().flags = xlMessageFlagErrorFrame
	errorObservation, err := decodeClassicReceiveEvent(&event, 2, timestamp)
	if err != nil || errorObservation.eventCount != 1 ||
		errorObservation.events[0].Kind != gocan.EventErrorFrame ||
		!errorObservation.requestChipState || errorObservation.terminal != nil {
		t.Fatalf("classic error observation = %+v, %v", errorObservation, err)
	}

	event = xlEvent{tag: xlEventChipState}
	*event.chipState() = xlChipState{
		busStatus:      xlChipStatePassive,
		txErrorCounter: 128,
		rxErrorCounter: 3,
	}
	stateObservation, err := decodeClassicReceiveEvent(&event, 2, timestamp)
	if err != nil || stateObservation.eventCount != 1 {
		t.Fatalf("classic state observation = %+v, %v", stateObservation, err)
	}
	state := stateObservation.events[0]
	if state.Kind != gocan.EventControllerState ||
		state.ControllerState != gocan.ControllerPassive ||
		state.TXErrorCount != 128 || state.RXErrorCount != 3 ||
		!state.ErrorCountsKnown || stateObservation.terminal != nil {
		t.Fatalf("classic passive state = %+v", stateObservation)
	}

	*event.chipState() = xlChipState{busStatus: xlChipStateBusOff}
	busOff, err := decodeClassicReceiveEvent(&event, 2, timestamp)
	if err != nil || !errors.Is(busOff.terminal, gocan.ErrBusOff) ||
		busOff.events[0].ControllerState != gocan.ControllerBusOff {
		t.Fatalf("classic bus-off = %+v, %v", busOff, err)
	}

	fdError, err := decodeFDReceiveEvent(
		&xlCANRXEvent{tag: xlCANEventTXError},
		2,
		timestamp,
	)
	if err != nil || fdError.events[0].Kind != gocan.EventErrorFrame ||
		!fdError.requestChipState {
		t.Fatalf("FD error observation = %+v, %v", fdError, err)
	}
}
