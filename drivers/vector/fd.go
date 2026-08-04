package vector

import (
	"fmt"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
)

const (
	xlInterfaceVersionFD = 4

	xlCANEventRXOK      = 1024
	xlCANEventRXError   = 1025
	xlCANEventTXError   = 1026
	xlCANEventChipState = 1033
	xlCANEventTX        = 1088

	xlCANMessageFlagEDL = 1
	xlCANMessageFlagBRS = 2
	xlCANMessageFlagESI = 4
	xlCANMessageFlagRTR = 16
	xlCANMessageFlagEF  = 512

	xlCANExtendedID uint32 = 1 << 31
)

// xlCANFDConfig mirrors XLcanFdConf from vxlapi.h.
type xlCANFDConfig struct {
	arbitrationBitrate uint32
	sjwArbitration     uint32
	tseg1Arbitration   uint32
	tseg2Arbitration   uint32
	dataBitrate        uint32
	sjwData            uint32
	tseg1Data          uint32
	tseg2Data          uint32
	reserved           uint8
	options            uint8
	reserved1          [2]uint8
	reserved2          uint8
}

func newXLCanFDConfig(config Config) xlCANFDConfig {
	return xlCANFDConfig{
		arbitrationBitrate: config.Bitrate,
		sjwArbitration:     2,
		tseg1Arbitration:   6,
		tseg2Arbitration:   3,
		dataBitrate:        config.DataBitrate,
		sjwData:            2,
		tseg1Data:          6,
		tseg2Data:          3,
	}
}

// xlCANRXMessage mirrors s_xl_can_ev_rx_msg from vxlapi.h.
type xlCANRXMessage struct {
	id            uint32
	flags         uint32
	crc           uint32
	reserved1     [12]uint8
	totalBitCount uint16
	dlc           uint8
	reserved2     [5]uint8
	data          [64]byte
}

// xlCANRXEvent mirrors XLcanRxEvent from vxlapi.h.
type xlCANRXEvent struct {
	size         int32
	tag          uint16
	channelIndex uint8
	reserved     uint8
	userHandle   int32
	chipFlags    uint16
	reserved0    uint16
	reserved1    int64
	timestamp    int64
	tagData      [96]byte
}

func (event *xlCANRXEvent) message() *xlCANRXMessage {
	return (*xlCANRXMessage)(unsafe.Pointer(&event.tagData[0]))
}

func (event *xlCANRXEvent) chipState() *xlChipState {
	return (*xlChipState)(unsafe.Pointer(&event.tagData[0]))
}

// xlCANTXMessage mirrors s_xl_can_tx_msg from vxlapi.h.
type xlCANTXMessage struct {
	id       uint32
	flags    uint32
	dlc      uint8
	reserved [7]uint8
	data     [64]byte
}

// xlCANTXEvent mirrors XLcanTxEvent from vxlapi.h. Its tag data union contains
// only xlCANTXMessage.
type xlCANTXEvent struct {
	tag           uint16
	transactionID uint16
	channelIndex  uint8
	reserved      [3]uint8
	message       xlCANTXMessage
}

func encodeFDTransmitEvent(frame gocan.Frame) xlCANTXEvent {
	event := xlCANTXEvent{
		tag:           xlCANEventTX,
		transactionID: 0xffff,
	}
	event.message.id = frame.ID
	if frame.Flags.Has(gocan.FrameExtended) {
		event.message.id |= xlCANExtendedID
	}
	if frame.Flags.Has(gocan.FrameFD) {
		event.message.flags |= xlCANMessageFlagEDL
	}
	if frame.Flags.Has(gocan.FrameBitRateSwitch) {
		event.message.flags |= xlCANMessageFlagBRS
	}
	if frame.Flags.Has(gocan.FrameRemote) {
		event.message.flags |= xlCANMessageFlagRTR
	}
	event.message.dlc = frame.DLC
	copy(event.message.data[:], frame.Data[:frame.DataLength()])
	return event
}

func decodeFDReceiveEvent(
	event *xlCANRXEvent,
	bus gocan.BusID,
	timestamp time.Time,
) (receiveObservation, error) {
	switch event.tag {
	case xlCANEventRXError, xlCANEventTXError:
		return newErrorObservation(bus, timestamp)
	case xlCANEventChipState:
		return newChipStateObservation(bus, timestamp, event.chipState())
	case xlCANEventRXOK:
	default:
		return receiveObservation{}, fmt.Errorf("unsupported Vector CAN FD event tag %d", event.tag)
	}

	message := event.message()
	if message.flags&xlCANMessageFlagEF != 0 {
		return newErrorObservation(bus, timestamp)
	}

	var flags gocan.FrameFlags
	if message.id&xlCANExtendedID != 0 {
		flags |= gocan.FrameExtended
	}
	if message.flags&xlCANMessageFlagEDL != 0 {
		flags |= gocan.FrameFD
	}
	if message.flags&xlCANMessageFlagBRS != 0 {
		flags |= gocan.FrameBitRateSwitch
	}
	if message.flags&xlCANMessageFlagESI != 0 {
		flags |= gocan.FrameErrorStateIndicator
	}
	if message.flags&xlCANMessageFlagRTR != 0 {
		flags |= gocan.FrameRemote
	}

	frame := gocan.Frame{
		ID:    message.id & gocan.MaxExtendedID,
		DLC:   message.dlc,
		Flags: flags,
	}
	if !flags.Has(gocan.FrameRemote) {
		length, err := gocan.DLCToLength(frame.DLC, flags.Has(gocan.FrameFD))
		if err != nil {
			return receiveObservation{}, fmt.Errorf("decode Vector CAN FD frame: %w", err)
		}
		copy(frame.Data[:], message.data[:length])
	}
	if err := frame.Validate(); err != nil {
		return receiveObservation{}, fmt.Errorf("decode Vector CAN FD frame: %w", err)
	}
	return receiveObservation{frame: frame, hasFrame: true}, nil
}
