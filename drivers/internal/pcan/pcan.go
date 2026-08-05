// Package pcan provides CAN access through PEAK-System PCAN-Basic.
// Received frames use host timestamps; native timestamps are not read.
package pcan

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
)

// Channel is a PCAN-Basic hardware channel handle.
type Channel uint16

const (
	// ChannelUSB1 is the first PCAN-USB channel.
	ChannelUSB1 Channel = 0x51
	// ChannelUSB2 is the second PCAN-USB channel.
	ChannelUSB2 Channel = 0x52
)

// Bitrate is a PCAN-Basic classical CAN BTR0BTR1 value.
type Bitrate uint16

const (
	// Bitrate500K selects 500 kbit/s classical CAN.
	Bitrate500K Bitrate = 0x001c
)

// Config selects and configures one PCAN-Basic channel.
type Config struct {
	// ID is the one-based trace channel assigned to the bus.
	ID gocan.BusID
	// Name is the human-readable name of the bus.
	Name string
	// Channel is the PCAN-Basic hardware channel handle.
	Channel Channel
	// Bitrate selects classical CAN. Set exactly one of Bitrate and FDBitrate.
	Bitrate Bitrate
	// FDBitrate is the vendor-native CAN FD timing string.
	FDBitrate string
}

type pcanStatus uint32

const (
	pcanStatusOK           pcanStatus = 0x00000
	pcanStatusTransmitFull pcanStatus = 0x00001
	pcanStatusOverrun      pcanStatus = 0x00002
	pcanStatusBusLight     pcanStatus = 0x00004
	pcanStatusBusWarning   pcanStatus = 0x00008
	pcanStatusBusOff       pcanStatus = 0x00010
	pcanStatusQueueEmpty   pcanStatus = 0x00020
	pcanStatusQueueOverrun pcanStatus = 0x00040
	pcanStatusQueueTXFull  pcanStatus = 0x00080
	pcanStatusInvalidHW    pcanStatus = 0x01400
	pcanStatusHandleMask   pcanStatus = 0x01c00
	pcanStatusInvalidData  pcanStatus = 0x20000
	pcanStatusBusPassive   pcanStatus = 0x40000
	pcanStatusCaution      pcanStatus = 0x2000000
)

const (
	pcanReceiveEvent      = 0x03
	pcanAllowStatusFrames = 0x1e
	pcanAllowErrorFrames  = 0x20
	pcanAttachedCount     = 0x2a
	pcanAttachedChannels  = 0x2b
	pcanFeatureFD         = 0x01

	pcanMessageRemote   = 0x01
	pcanMessageExtended = 0x02
	pcanMessageFD       = 0x04
	pcanMessageBRS      = 0x08
	pcanMessageESI      = 0x10
	pcanMessageEcho     = 0x20
	pcanMessageError    = 0x40
	pcanMessageStatus   = 0x80
)

type pcanMsg struct {
	id          uint32
	messageType uint8
	length      uint8
	data        [8]byte
}

type pcanMsgFD struct {
	id          uint32
	messageType uint8
	dlc         uint8
	data        [64]byte
}

type pcanChannelInformation struct {
	channelHandle    Channel
	deviceType       uint8
	controllerNumber uint8
	deviceFeatures   uint32
	deviceName       [33]byte
	_                [3]byte
	deviceID         uint32
	channelCondition uint32
}

type pcanReceiveObservation struct {
	frame      gocan.Frame
	hasFrame   bool
	events     [3]gocan.Event
	eventCount uint8
	terminal   error
}

func (observation *pcanReceiveObservation) addEvent(event gocan.Event) {
	observation.events[observation.eventCount] = event
	observation.eventCount++
}

func validateConfig(capture *gocan.Capture, config Config) error {
	switch {
	case capture == nil:
		return errors.New("PCAN bus requires a capture")
	case config.ID == 0:
		return errors.New("PCAN bus requires an ID")
	case config.Name == "":
		return errors.New("PCAN bus requires a name")
	case config.Channel == 0:
		return errors.New("PCAN bus requires a channel")
	case (config.Bitrate == 0) == (config.FDBitrate == ""):
		return errors.New("PCAN bus requires exactly one of Bitrate and FDBitrate")
	}
	return nil
}

func validateSendFrame(frame gocan.Frame, fdAPI bool) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.Flags.Has(gocan.FrameFD) && !fdAPI {
		return errors.New("PCAN classical bus cannot send a CAN FD frame")
	}
	// TPCANMsg's LEN member is limited to 0..8. TPCANMsgFD carries the full
	// four-bit DLC even when the EDL/FD flag is clear.
	if !fdAPI && frame.DLC > 8 {
		return fmt.Errorf("PCAN classical API cannot send DLC %d", frame.DLC)
	}
	return nil
}

func encodeClassicMessage(frame gocan.Frame) pcanMsg {
	message := pcanMsg{
		id:          frame.ID,
		messageType: encodeMessageType(frame),
		length:      frame.DLC,
	}
	if !frame.Flags.Has(gocan.FrameRemote) {
		copy(message.data[:], frame.Data[:frame.DataLength()])
	}
	return message
}

func encodeFDMessage(frame gocan.Frame) pcanMsgFD {
	message := pcanMsgFD{
		id:          frame.ID,
		messageType: encodeMessageType(frame),
		dlc:         frame.DLC,
	}
	if !frame.Flags.Has(gocan.FrameRemote) {
		copy(message.data[:], frame.Data[:frame.DataLength()])
	}
	return message
}

func encodeMessageType(frame gocan.Frame) uint8 {
	var messageType uint8
	if frame.Flags.Has(gocan.FrameRemote) {
		messageType |= pcanMessageRemote
	}
	if frame.Flags.Has(gocan.FrameExtended) {
		messageType |= pcanMessageExtended
	}
	if frame.Flags.Has(gocan.FrameFD) {
		messageType |= pcanMessageFD
	}
	if frame.Flags.Has(gocan.FrameBitRateSwitch) {
		messageType |= pcanMessageBRS
	}
	if frame.Flags.Has(gocan.FrameErrorStateIndicator) {
		messageType |= pcanMessageESI
	}
	return messageType
}

func decodeClassicMessage(message pcanMsg) (gocan.Frame, error) {
	return decodeMessage(
		message.id,
		message.messageType,
		message.length,
		message.data[:],
		false,
	)
}

func decodeFDMessage(message pcanMsgFD) (gocan.Frame, error) {
	return decodeMessage(
		message.id,
		message.messageType,
		message.dlc,
		message.data[:],
		true,
	)
}

func decodeStatusFrame(messageType uint8, data []byte) (pcanStatus, bool) {
	if messageType&pcanMessageStatus == 0 {
		return 0, false
	}
	return pcanStatus(binary.BigEndian.Uint32(data[:4])), true
}

func decodePCANReceive(
	id uint32,
	messageType uint8,
	dlc uint8,
	data []byte,
	fdAPI bool,
	status pcanStatus,
	bus gocan.BusID,
	timestamp time.Time,
) (pcanReceiveObservation, error) {
	if frameStatus, ok := decodeStatusFrame(messageType, data); ok {
		return decodePCANStatus(frameStatus, bus, timestamp)
	}
	if messageType&pcanMessageError != 0 {
		return decodePCANErrorFrame(data, status, bus, timestamp)
	}
	if status != pcanStatusOK {
		return decodePCANStatus(status, bus, timestamp)
	}
	frame, err := decodeMessage(id, messageType, dlc, data, fdAPI)
	if err != nil {
		return pcanReceiveObservation{}, err
	}
	return pcanReceiveObservation{frame: frame, hasFrame: true}, nil
}

func decodePCANErrorFrame(
	data []byte,
	status pcanStatus,
	bus gocan.BusID,
	timestamp time.Time,
) (pcanReceiveObservation, error) {
	if len(data) < 4 {
		return pcanReceiveObservation{}, fmt.Errorf("PCAN error frame has %d data bytes", len(data))
	}
	errorEvent, err := gocan.NewErrorFrameEvent(bus, timestamp)
	if err != nil {
		return pcanReceiveObservation{}, err
	}
	var observation pcanReceiveObservation
	observation.addEvent(errorEvent)

	if status&pcanStatusBusOff != 0 {
		state, err := gocan.NewControllerStateEvent(
			bus,
			timestamp,
			gocan.ControllerBusOff,
			0,
			0,
			false,
		)
		if err != nil {
			return pcanReceiveObservation{}, err
		}
		observation.addEvent(state)
		observation.terminal = fmt.Errorf("%w: PCAN controller entered bus-off", gocan.ErrBusOff)
		return observation, nil
	}

	rxErrorCount := data[2]
	txErrorCount := data[3]
	state := gocan.ControllerActive
	if txErrorCount >= 128 || rxErrorCount >= 128 {
		state = gocan.ControllerPassive
	} else if txErrorCount >= 96 || rxErrorCount >= 96 {
		state = gocan.ControllerWarning
	}
	stateEvent, err := gocan.NewControllerStateEvent(
		bus,
		timestamp,
		state,
		txErrorCount,
		rxErrorCount,
		true,
	)
	if err != nil {
		return pcanReceiveObservation{}, err
	}
	observation.addEvent(stateEvent)
	return observation, nil
}

func decodePCANStatus(
	status pcanStatus,
	bus gocan.BusID,
	timestamp time.Time,
) (pcanReceiveObservation, error) {
	if status&(pcanStatusOverrun|pcanStatusQueueOverrun) != 0 {
		event, err := gocan.NewReceiveOverrunEvent(bus, timestamp)
		if err != nil {
			return pcanReceiveObservation{}, err
		}
		observation := pcanReceiveObservation{
			terminal: fmt.Errorf("%w: PCAN status %#08x", gocan.ErrReceiveOverrun, uint32(status)),
		}
		observation.addEvent(event)
		return observation, nil
	}

	var state gocan.ControllerState
	switch {
	case status&pcanStatusBusOff != 0:
		state = gocan.ControllerBusOff
	case status&pcanStatusBusPassive != 0:
		state = gocan.ControllerPassive
	case status&(pcanStatusBusLight|pcanStatusBusWarning) != 0:
		state = gocan.ControllerWarning
	case status == pcanStatusOK:
		state = gocan.ControllerActive
	default:
		return pcanReceiveObservation{}, fmt.Errorf("unsupported PCAN receive status %#08x", uint32(status))
	}
	event, err := gocan.NewControllerStateEvent(bus, timestamp, state, 0, 0, false)
	if err != nil {
		return pcanReceiveObservation{}, err
	}
	var observation pcanReceiveObservation
	observation.addEvent(event)
	if state == gocan.ControllerBusOff {
		observation.terminal = fmt.Errorf("%w: PCAN controller entered bus-off", gocan.ErrBusOff)
	}
	return observation, nil
}

func decodeMessage(id uint32, messageType, dlc uint8, data []byte, fdAPI bool) (gocan.Frame, error) {
	if unsupported := messageType & (pcanMessageEcho | pcanMessageError | pcanMessageStatus); unsupported != 0 {
		return gocan.Frame{}, fmt.Errorf("unsupported PCAN message type %#02x", messageType)
	}

	allowed := uint8(pcanMessageRemote | pcanMessageExtended)
	if fdAPI {
		allowed |= pcanMessageFD | pcanMessageBRS | pcanMessageESI
	}
	if unknown := messageType &^ allowed; unknown != 0 {
		return gocan.Frame{}, fmt.Errorf("unsupported PCAN message type %#02x", messageType)
	}

	var flags gocan.FrameFlags
	if messageType&pcanMessageRemote != 0 {
		flags |= gocan.FrameRemote
	}
	if messageType&pcanMessageExtended != 0 {
		flags |= gocan.FrameExtended
	}
	if messageType&pcanMessageFD != 0 {
		flags |= gocan.FrameFD
	}
	if messageType&pcanMessageBRS != 0 {
		flags |= gocan.FrameBitRateSwitch
	}
	if messageType&pcanMessageESI != 0 {
		flags |= gocan.FrameErrorStateIndicator
	}

	frame := gocan.Frame{ID: id, DLC: dlc, Flags: flags}
	if !flags.Has(gocan.FrameRemote) {
		length, err := gocan.DLCToLength(dlc, flags.Has(gocan.FrameFD))
		if err != nil {
			return gocan.Frame{}, fmt.Errorf("decode PCAN frame: %w", err)
		}
		if length > len(data) {
			return gocan.Frame{}, fmt.Errorf("decode PCAN frame: DLC %d needs %d data bytes", dlc, length)
		}
		copy(frame.Data[:], data[:length])
	}
	if err := frame.Validate(); err != nil {
		return gocan.Frame{}, fmt.Errorf("decode PCAN frame: %w", err)
	}
	return frame, nil
}
