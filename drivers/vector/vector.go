// Package vector provides classical CAN and ISO CAN FD access through the
// Vector XL Driver Library.
// Received frames use host timestamps; native timestamps are not read.
package vector

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
)

// ChannelIndex is a global channel index reported by the XL Driver Library.
type ChannelIndex uint8

// Config selects and configures one Vector CAN channel.
type Config struct {
	// ID is the one-based trace channel assigned to the bus.
	ID gocan.BusID
	// Name is the human-readable name of the bus.
	Name string
	// ChannelIndex is the global XL Driver Library channel index.
	ChannelIndex ChannelIndex
	// Bitrate is the classic bitrate, or the CAN FD arbitration-phase bitrate,
	// in bits per second.
	Bitrate uint32
	// DataBitrate enables CAN FD and selects its data-phase bitrate in bits per
	// second. Zero selects a classic CAN channel. Bit-timing segments are fixed.
	DataBitrate uint32
}

func (config Config) fd() bool { return config.DataBitrate != 0 }

type xlStatus int16
type xlPortHandle int32
type xlAccess uint64

const (
	xlBusTypeCAN       = 1
	xlInterfaceVersion = 3
	xlReceiveQueueSize = 1 << 14

	xlEventReceiveMessage  = 1
	xlEventChipState       = 4
	xlEventTransmitMessage = 10

	xlEventFlagOverrun = 1

	xlMessageFlagErrorFrame         = 1
	xlMessageFlagOverrun            = 2
	xlMessageFlagNERR               = 4
	xlMessageFlagWakeup             = 8
	xlMessageFlagRemote             = 16
	xlMessageFlagTXCompleted        = 64
	xlMessageFlagTXRequest          = 128
	xlMessageFlagSRRBitDom          = 512
	xlMessageExtendedID      uint32 = 1 << 31

	xlChipStateBusOff  = 1
	xlChipStatePassive = 2
	xlChipStateWarning = 4
	xlChipStateActive  = 8
)

const xlUnsupportedMessageFlags = xlMessageFlagErrorFrame |
	xlMessageFlagNERR |
	xlMessageFlagWakeup |
	xlMessageFlagTXCompleted |
	xlMessageFlagTXRequest |
	xlMessageFlagSRRBitDom

// xlCanMessage mirrors s_xl_can_msg from vxlapi.h.
type xlCanMessage struct {
	id        uint32
	flags     uint16
	dlc       uint16
	reserved1 int64
	data      [8]byte
	reserved2 int64
}

// xlEvent mirrors XLevent from vxlapi.h. tagData is the 32-byte classic CAN
// event union.
type xlEvent struct {
	tag           uint8
	channelIndex  uint8
	transactionID uint16
	portHandle    uint16
	flags         uint8
	reserved      uint8
	timestamp     int64
	tagData       [32]byte
}

func (event *xlEvent) message() *xlCanMessage {
	return (*xlCanMessage)(unsafe.Pointer(&event.tagData[0]))
}

type xlChipState struct {
	busStatus      uint8
	txErrorCounter uint8
	rxErrorCounter uint8
}

func (event *xlEvent) chipState() *xlChipState {
	return (*xlChipState)(unsafe.Pointer(&event.tagData[0]))
}

// receiveObservation is the Vector-specific result of one native receive
// event. A native record may represent a frame, one or more bus events, or a
// terminal condition that must be appended before the bus stops.
type receiveObservation struct {
	frame            gocan.Frame
	hasFrame         bool
	events           [2]gocan.Event
	eventCount       uint8
	terminal         error
	requestChipState bool
}

func (observation *receiveObservation) addEvent(event gocan.Event) {
	observation.events[observation.eventCount] = event
	observation.eventCount++
}

func newErrorObservation(bus gocan.BusID, timestamp time.Time) (receiveObservation, error) {
	event, err := gocan.NewErrorFrameEvent(bus, timestamp)
	if err != nil {
		return receiveObservation{}, err
	}
	var observation receiveObservation
	observation.addEvent(event)
	observation.requestChipState = true
	return observation, nil
}

func newOverrunObservation(bus gocan.BusID, timestamp time.Time, detail string) (receiveObservation, error) {
	event, err := gocan.NewReceiveOverrunEvent(bus, timestamp)
	if err != nil {
		return receiveObservation{}, err
	}
	var observation receiveObservation
	observation.addEvent(event)
	observation.terminal = fmt.Errorf("%w: %s", gocan.ErrReceiveOverrun, detail)
	return observation, nil
}

func newChipStateObservation(
	bus gocan.BusID,
	timestamp time.Time,
	state *xlChipState,
) (receiveObservation, error) {
	controllerState, err := decodeChipState(state.busStatus)
	if err != nil {
		return receiveObservation{}, err
	}
	event, err := gocan.NewControllerStateEvent(
		bus,
		timestamp,
		controllerState,
		state.txErrorCounter,
		state.rxErrorCounter,
		true,
	)
	if err != nil {
		return receiveObservation{}, err
	}
	var observation receiveObservation
	observation.addEvent(event)
	if controllerState == gocan.ControllerBusOff {
		observation.terminal = fmt.Errorf("%w: Vector controller entered bus-off", gocan.ErrBusOff)
	}
	return observation, nil
}

func decodeChipState(status uint8) (gocan.ControllerState, error) {
	switch status {
	case xlChipStateActive:
		return gocan.ControllerActive, nil
	case xlChipStateWarning:
		return gocan.ControllerWarning, nil
	case xlChipStatePassive:
		return gocan.ControllerPassive, nil
	case xlChipStateBusOff:
		return gocan.ControllerBusOff, nil
	default:
		return 0, fmt.Errorf("unsupported Vector chip state %#02x", status)
	}
}

func validateConfig(capture *gocan.Capture, config Config) error {
	switch {
	case capture == nil:
		return errors.New("Vector bus requires a capture")
	case config.ID == 0:
		return errors.New("Vector bus requires an ID")
	case config.Name == "":
		return errors.New("Vector bus requires a name")
	case config.ChannelIndex >= 64:
		return fmt.Errorf("Vector channel index %d exceeds 63", config.ChannelIndex)
	case config.Bitrate == 0:
		return errors.New("Vector bus requires a bitrate")
	}
	return nil
}

func channelAccess(index ChannelIndex) xlAccess {
	return xlAccess(uint64(1) << index)
}

func encodeEvent(frame gocan.Frame) xlEvent {
	event := xlEvent{tag: xlEventTransmitMessage}
	message := event.message()
	message.id = frame.ID
	if frame.Flags.Has(gocan.FrameExtended) {
		message.id |= xlMessageExtendedID
	}
	if frame.Flags.Has(gocan.FrameRemote) {
		message.flags |= xlMessageFlagRemote
	}
	message.dlc = uint16(frame.DLC)
	if !frame.Flags.Has(gocan.FrameRemote) {
		copy(message.data[:], frame.Data[:frame.DataLength()])
	}
	return event
}

func decodeClassicReceiveEvent(
	event *xlEvent,
	bus gocan.BusID,
	timestamp time.Time,
) (receiveObservation, error) {
	if event.flags&xlEventFlagOverrun != 0 {
		return newOverrunObservation(bus, timestamp, "Vector event queue overrun")
	}
	switch event.tag {
	case xlEventChipState:
		return newChipStateObservation(bus, timestamp, event.chipState())
	case xlEventReceiveMessage:
	default:
		return receiveObservation{}, fmt.Errorf("unsupported Vector event tag %d", event.tag)
	}

	message := event.message()
	if message.flags&xlMessageFlagOverrun != 0 {
		return newOverrunObservation(bus, timestamp, "Vector message queue overrun")
	}
	if message.flags&xlMessageFlagErrorFrame != 0 {
		return newErrorObservation(bus, timestamp)
	}
	if unsupported := message.flags & xlUnsupportedMessageFlags; unsupported != 0 {
		return receiveObservation{}, fmt.Errorf("unsupported Vector message flags %#04x", unsupported)
	}
	if message.dlc > 15 {
		return receiveObservation{}, fmt.Errorf("classic Vector frame has DLC %d", message.dlc)
	}

	var flags gocan.FrameFlags
	if message.id&xlMessageExtendedID != 0 {
		flags |= gocan.FrameExtended
	}
	if message.flags&xlMessageFlagRemote != 0 {
		flags |= gocan.FrameRemote
	}

	frame := gocan.Frame{
		ID:    message.id & gocan.MaxExtendedID,
		DLC:   uint8(message.dlc),
		Flags: flags,
	}
	if !flags.Has(gocan.FrameRemote) {
		length := min(int(frame.DLC), 8)
		copy(frame.Data[:], message.data[:length])
	}
	if err := frame.Validate(); err != nil {
		return receiveObservation{}, fmt.Errorf("decode Vector frame: %w", err)
	}
	return receiveObservation{frame: frame, hasFrame: true}, nil
}
