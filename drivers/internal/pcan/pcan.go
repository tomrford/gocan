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

type Channel uint16

const (
	ChannelUSB1 Channel = 0x51
	ChannelUSB2 Channel = 0x52
)

// Bus returns the channel's one-based number within its PCAN bus type, as in
// PCAN_USBBUS1. PCANBasic.h numbers buses 1–8 in the handle's low nibble and
// buses 9–16 in the low byte of a 0x?xx handle.
func (channel Channel) Bus() int {
	if channel > 0xff {
		return int(channel & 0xff)
	}
	return int(channel & 0x0f)
}

// classicalBitrates maps a classical bitrate in bits per second to the
// PCAN-Basic predefined BTR0BTR1 code (PCANBasic.h PCAN_BAUD_*). BTR0BTR1
// divides the SJA1000's 8 MHz quantum clock, so four PEAK rates are not
// whole numbers; they are keyed by the exact rate rounded to the nearest
// bit per second (for example 8 MHz / 84 becomes 95_238).
var classicalBitrates = map[uint32]uint16{
	5_000:     0x7f7f,
	10_000:    0x672f,
	20_000:    0x532f,
	33_333:    0x8b2f,
	47_619:    0x1414,
	50_000:    0x472f,
	83_333:    0x852b,
	95_238:    0xc34e,
	100_000:   0x432f,
	125_000:   0x031c,
	250_000:   0x011c,
	500_000:   0x001c,
	800_000:   0x0016,
	1_000_000: 0x0014,
}

// ValidClassicalBitrate reports whether bitrate is a predefined PCAN-Basic
// classical CAN rate.
func ValidClassicalBitrate(bitrate uint32) bool {
	_, ok := classicalBitrates[bitrate]
	return ok
}

type Config struct {
	ID      gocan.BusID
	Name    string
	Channel Channel
	// Bitrate selects one of the PCAN-Basic predefined classical rates in
	// bits per second. Set exactly one of Bitrate and FDBitrate.
	Bitrate uint32
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

	pcanStatusOverrunMask  = pcanStatusOverrun | pcanStatusQueueOverrun
	pcanStatusBusStateMask = pcanStatusBusLight | pcanStatusBusWarning | pcanStatusBusPassive | pcanStatusBusOff
	pcanStatusTXFullMask   = pcanStatusTransmitFull | pcanStatusQueueTXFull
	pcanStatusKnownMask    = pcanStatusBusStateMask | pcanStatusOverrunMask | pcanStatusTXFullMask | pcanStatusQueueEmpty
)

const (
	pcanReceiveEvent      = 0x03
	pcanBusOffAutoReset   = 0x07
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
	busHeavy   bool
	events     [3]gocan.Event
	eventCount uint8
}

func (observation *pcanReceiveObservation) addEvent(event gocan.Event) {
	observation.events[observation.eventCount] = event
	observation.eventCount++
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
) (pcanReceiveObservation, error) {
	if frameStatus, ok := decodeStatusFrame(messageType, data); ok {
		return decodePCANStatus(frameStatus, bus)
	}
	if messageType&pcanMessageError != 0 {
		return decodePCANErrorFrame(data, status, bus)
	}
	if status != pcanStatusOK {
		return decodePCANStatus(status, bus)
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
) (pcanReceiveObservation, error) {
	if len(data) < 4 {
		return pcanReceiveObservation{}, fmt.Errorf("PCAN error frame has %d data bytes", len(data))
	}
	errorEvent := gocan.Event{Bus: bus, Kind: gocan.EventErrorFrame}
	var observation pcanReceiveObservation
	observation.addEvent(errorEvent)

	if status&pcanStatusBusOff != 0 {
		state := gocan.Event{
			Bus:             bus,
			Kind:            gocan.EventControllerState,
			ControllerState: gocan.ControllerBusOff,
		}
		observation.addEvent(state)
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
	stateEvent := gocan.Event{
		Bus:              bus,
		Kind:             gocan.EventControllerState,
		ControllerState:  state,
		TXErrorCount:     txErrorCount,
		RXErrorCount:     rxErrorCount,
		ErrorCountsKnown: true,
	}
	observation.addEvent(stateEvent)
	return observation, nil
}

func decodePCANStatus(
	status pcanStatus,
	bus gocan.BusID,
) (pcanReceiveObservation, error) {
	if status&^pcanStatusKnownMask != 0 {
		return pcanReceiveObservation{}, fmt.Errorf("unsupported PCAN receive status %#08x", uint32(status))
	}
	var observation pcanReceiveObservation
	if status&pcanStatusOverrunMask != 0 {
		observation.addEvent(gocan.Event{Bus: bus, Kind: gocan.EventReceiveOverrun})
	}

	busState := status & pcanStatusBusStateMask
	var state gocan.ControllerState
	switch {
	case busState&pcanStatusBusOff != 0:
		state = gocan.ControllerBusOff
	case busState&pcanStatusBusPassive != 0:
		state = gocan.ControllerPassive
	case busState&(pcanStatusBusLight|pcanStatusBusWarning) != 0:
		state = gocan.ControllerWarning
		observation.busHeavy = busState&pcanStatusBusWarning != 0
	case status == pcanStatusOK:
		state = gocan.ControllerActive
	default:
		return observation, nil
	}
	event := gocan.Event{
		Bus:             bus,
		Kind:            gocan.EventControllerState,
		ControllerState: state,
	}
	observation.addEvent(event)
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

const (
	// busOffPollInterval paces CAN_GetStatus reads during bus-off recovery.
	// Keeping any write re-probe at least 50 ms apart stays well above the
	// <= 20 ms write interval observed trapping PCAN-USB in bus-off.
	busOffPollInterval = 50 * time.Millisecond

	// degradedQuietWindow is how long a warning or passive channel must go
	// without errors after clean received traffic before inferring error-active.
	degradedQuietWindow = 500 * time.Millisecond
)

// pcanState tracks PCAN controller-state transitions, bus-off write gating, and
// quiet-window active inference independent of Windows APIs.
type pcanState struct {
	lastState   gocan.Event
	inBusOff    bool
	pollOverrun bool
	nextPoll    time.Time
	activeAt    time.Time
}

func (state *pcanState) busOff() bool {
	return state.inBusOff
}

func (state *pcanState) nextDeadline() time.Time {
	if !state.nextPoll.IsZero() {
		return state.nextPoll
	}
	return state.activeAt
}

func (state *pcanState) degraded() bool {
	return state.lastState.Kind == gocan.EventControllerState &&
		state.lastState.ControllerState != gocan.ControllerActive
}

func (state *pcanState) observeSendBusOff(now time.Time, bus gocan.BusID) (gocan.Event, bool) {
	event := gocan.Event{
		Bus:             bus,
		Kind:            gocan.EventControllerState,
		ControllerState: gocan.ControllerBusOff,
	}
	state.inBusOff = true
	state.activeAt = time.Time{}
	if state.nextPoll.IsZero() {
		state.pollOverrun = false
		state.nextPoll = now.Add(busOffPollInterval)
	}
	if samePCANControllerState(state.lastState, event) {
		return gocan.Event{}, false
	}
	state.lastState = event
	return event, true
}

func (state *pcanState) observe(now time.Time, observation pcanReceiveObservation) ([3]gocan.Event, uint8) {
	var out [3]gocan.Event
	var count uint8
	for _, event := range observation.events[:observation.eventCount] {
		switch event.Kind {
		case gocan.EventErrorFrame:
			state.activeAt = time.Time{}
			out[count] = event
			count++
		case gocan.EventControllerState:
			if event.ControllerState == gocan.ControllerBusOff {
				if recorded, ok := state.observeSendBusOff(now, event.Bus); ok {
					out[count] = recorded
					count++
				}
				continue
			}
			if state.inBusOff {
				// Queued non-bus-off frames/status cannot leave bus-off; only
				// live CAN_GetStatus polling releases the bus-off gate.
				continue
			}
			if !state.nextPoll.IsZero() && event.ControllerState == gocan.ControllerActive {
				// Queued active status or low-counter error frames from before
				// bus-off cannot end recovery polling; wait for a live
				// CAN_GetStatus read to confirm error-active.
				continue
			}
			if state.lastState.ControllerState == gocan.ControllerPassive &&
				observation.busHeavy && !event.ErrorCountsKnown {
				// Classic PCAN-USB reports BUSHEAVY (0x08) while error-passive.
				// Still reset the quiet window since hardware reports an active fault.
				state.activeAt = time.Time{}
				continue
			}
			state.activeAt = time.Time{}
			if samePCANControllerState(state.lastState, event) {
				continue
			}
			state.lastState = event
			out[count] = event
			count++
		default:
			out[count] = event
			count++
		}
	}
	if observation.hasFrame && !state.inBusOff && state.nextPoll.IsZero() && state.degraded() && state.activeAt.IsZero() {
		state.activeAt = now.Add(degradedQuietWindow)
	}
	return out, count
}

func (state *pcanState) pollStatus(now time.Time, status pcanStatus, bus gocan.BusID) ([2]gocan.Event, uint8, error) {
	if status&^pcanStatusKnownMask != 0 {
		return [2]gocan.Event{}, 0, fmt.Errorf("unsupported PCAN status %#08x", uint32(status))
	}
	var out [2]gocan.Event
	var count uint8
	if status&pcanStatusOverrunMask != 0 {
		if !state.pollOverrun {
			state.pollOverrun = true
			out[count] = gocan.Event{Bus: bus, Kind: gocan.EventReceiveOverrun}
			count++
		}
	} else {
		state.pollOverrun = false
	}
	if status&pcanStatusBusOff != 0 {
		state.inBusOff = true
		state.activeAt = time.Time{}
		state.nextPoll = now.Add(busOffPollInterval)
		event := gocan.Event{Bus: bus, Kind: gocan.EventControllerState, ControllerState: gocan.ControllerBusOff}
		if !samePCANControllerState(state.lastState, event) {
			state.lastState = event
			out[count] = event
			count++
		}
		return out, count, nil
	}

	// Bus-off bit cleared: release the write gate so persistent warning,
	// passive, overrun, or TX-full bits cannot trap Send indefinitely. Keep
	// polling while a post-bus-off degraded bus-state bit remains set so an
	// idle bus still reports the transition to error-active.
	state.inBusOff = false
	busState := status & pcanStatusBusStateMask
	nextState := gocan.ControllerActive
	switch {
	case busState&pcanStatusBusPassive != 0:
		nextState = gocan.ControllerPassive
	case busState&(pcanStatusBusLight|pcanStatusBusWarning) != 0:
		nextState = gocan.ControllerWarning
	}
	if busState != 0 {
		state.nextPoll = now.Add(busOffPollInterval)
		state.activeAt = time.Time{}
	} else {
		state.nextPoll = time.Time{}
		state.activeAt = time.Time{}
		state.pollOverrun = false
	}
	if state.lastState.ControllerState == gocan.ControllerPassive &&
		busState&pcanStatusBusWarning != 0 && busState&pcanStatusBusPassive == 0 {
		// Classic PCAN-USB reports BUSHEAVY (0x08) while error-passive; keep
		// polling for recovery without downgrading a known passive state.
		return out, count, nil
	}
	event := gocan.Event{Bus: bus, Kind: gocan.EventControllerState, ControllerState: nextState}
	if !samePCANControllerState(state.lastState, event) {
		state.lastState = event
		out[count] = event
		count++
	}
	return out, count, nil
}

func (state *pcanState) tick(now time.Time, bus gocan.BusID) (gocan.Event, bool) {
	if state.inBusOff || !state.nextPoll.IsZero() || state.activeAt.IsZero() || now.Before(state.activeAt) {
		return gocan.Event{}, false
	}
	state.activeAt = time.Time{}
	state.nextPoll = time.Time{}
	state.pollOverrun = false
	event := gocan.Event{Bus: bus, Kind: gocan.EventControllerState, ControllerState: gocan.ControllerActive}
	if samePCANControllerState(state.lastState, event) {
		return gocan.Event{}, false
	}
	state.lastState = event
	return event, true
}

func samePCANControllerState(previous, next gocan.Event) bool {
	if previous.Kind != gocan.EventControllerState || previous.ControllerState != next.ControllerState {
		return false
	}
	if !next.ErrorCountsKnown {
		return true
	}
	return previous.ErrorCountsKnown &&
		previous.TXErrorCount == next.TXErrorCount &&
		previous.RXErrorCount == next.RXErrorCount
}
