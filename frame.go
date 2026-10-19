// Package gocan provides raw CAN and CAN FD values, capture storage, and the
// common vocabulary used by hardware drivers and higher protocol packages.
//
// The package is under initial development. Its public API may change before
// version 1.
package gocan

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// MaxStandardID is the largest 11-bit CAN arbitration identifier.
	MaxStandardID uint32 = 0x7ff
	// MaxExtendedID is the largest 29-bit CAN arbitration identifier.
	MaxExtendedID uint32 = 0x1fffffff
	// MaxDataLength is the largest CAN FD payload in bytes.
	MaxDataLength = 64
)

// ErrInvalidFrame identifies an invalid CAN frame.
var ErrInvalidFrame = errors.New("invalid CAN frame")

// FrameFlags describe properties of a CAN frame.
type FrameFlags uint8

const (
	// FrameExtended indicates a 29-bit arbitration identifier.
	FrameExtended FrameFlags = 1 << iota
	// FrameRemote indicates a classical CAN remote transmission request.
	FrameRemote
	// FrameFD indicates a CAN FD frame.
	FrameFD
	// FrameBitRateSwitch indicates CAN FD bit-rate switching.
	FrameBitRateSwitch
	// FrameErrorStateIndicator indicates the CAN FD error state indicator.
	FrameErrorStateIndicator
)

const allFrameFlags = FrameExtended | FrameRemote | FrameFD | FrameBitRateSwitch | FrameErrorStateIndicator

// hexDigits renders identifiers, data length codes, and payload bytes in the
// uppercase form trace logs use.
const hexDigits = "0123456789ABCDEF"

// Has reports whether flags contains flag.
func (flags FrameFlags) Has(flag FrameFlags) bool {
	return flags&flag != 0
}

// String returns the names of the flags that are set, separated by "|", or the
// empty string when none is set. An undefined bit renders as its hexadecimal
// value. For example, "extended|fd|brs".
func (flags FrameFlags) String() string {
	names := make([]string, 0, 5)
	for _, named := range [...]struct {
		flag FrameFlags
		name string
	}{
		{FrameExtended, "extended"},
		{FrameRemote, "remote"},
		{FrameFD, "fd"},
		{FrameBitRateSwitch, "brs"},
		{FrameErrorStateIndicator, "esi"},
	} {
		if flags.Has(named.flag) {
			names = append(names, named.name)
		}
	}
	if unknown := flags &^ allFrameFlags; unknown != 0 {
		names = append(names, fmt.Sprintf("%#x", uint8(unknown)))
	}
	return strings.Join(names, "|")
}

// Frame is an owned classical CAN or CAN FD frame.
//
// DLC is the on-wire data length code. Use DataLength to obtain the payload
// length in bytes. Bytes in Data beyond DataLength are not part of the frame.
type Frame struct {
	ID    uint32
	Data  [MaxDataLength]byte
	DLC   uint8
	Flags FrameFlags
}

// NewFrame constructs and validates a frame, copying data into owned storage.
func NewFrame(id uint32, data []byte, flags FrameFlags) (Frame, error) {
	if flags.Has(FrameRemote) {
		return Frame{}, fmt.Errorf("%w: use NewRemoteFrame to construct a remote frame", ErrInvalidFrame)
	}
	dlc, err := LengthToDLC(len(data), flags.Has(FrameFD))
	if err != nil {
		return Frame{}, err
	}

	frame := Frame{
		ID:    id,
		DLC:   dlc,
		Flags: flags,
	}
	copy(frame.Data[:], data)
	if err := frame.Validate(); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

// NewRemoteFrame constructs and validates a classical CAN remote frame.
func NewRemoteFrame(id uint32, dlc uint8, extended bool) (Frame, error) {
	flags := FrameRemote
	if extended {
		flags |= FrameExtended
	}
	frame := Frame{ID: id, DLC: dlc, Flags: flags}
	if err := frame.Validate(); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

// DataLength returns the frame payload length in bytes.
//
// Remote frames carry no payload even though their DLC describes the requested
// data length.
func (frame Frame) DataLength() int {
	if frame.Flags.Has(FrameRemote) {
		return 0
	}
	length, err := DLCToLength(frame.DLC, frame.Flags.Has(FrameFD))
	if err != nil {
		return 0
	}
	return length
}

// Validate reports whether frame is a valid classical CAN or CAN FD frame.
func (frame Frame) Validate() error {
	if unknown := frame.Flags &^ allFrameFlags; unknown != 0 {
		return fmt.Errorf("%w: unknown frame flags %#x", ErrInvalidFrame, uint8(unknown))
	}
	if frame.Flags.Has(FrameExtended) {
		if frame.ID > MaxExtendedID {
			return fmt.Errorf("%w: extended identifier %#x exceeds %#x", ErrInvalidFrame, frame.ID, MaxExtendedID)
		}
	} else if frame.ID > MaxStandardID {
		return fmt.Errorf("%w: standard identifier %#x exceeds %#x", ErrInvalidFrame, frame.ID, MaxStandardID)
	}

	if frame.DLC > 15 {
		return fmt.Errorf("%w: DLC %d exceeds 15", ErrInvalidFrame, frame.DLC)
	}
	if frame.Flags.Has(FrameRemote) && frame.Flags.Has(FrameFD) {
		return fmt.Errorf("%w: CAN FD does not support remote frames", ErrInvalidFrame)
	}
	if frame.Flags.Has(FrameBitRateSwitch) && !frame.Flags.Has(FrameFD) {
		return fmt.Errorf("%w: bit-rate switching requires CAN FD", ErrInvalidFrame)
	}
	if frame.Flags.Has(FrameErrorStateIndicator) && !frame.Flags.Has(FrameFD) {
		return fmt.Errorf("%w: error state indicator requires CAN FD", ErrInvalidFrame)
	}
	if _, err := DLCToLength(frame.DLC, frame.Flags.Has(FrameFD)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidFrame, err)
	}
	return nil
}

// String returns frame as a can-utils log line: the form written by "candump -L"
// and read by candump, cantraceviewer, and python-can's CanutilsLogWriter. A
// standard identifier uses three hexadecimal digits and an extended identifier
// eight, a remote frame renders its requested data length code after "R", CAN FD
// adds a flag digit carrying bit-rate switching and the error state indicator,
// and a classical frame whose data length code exceeds eight appends "_<dlc>".
//
// This is a log form, not a canonical serialization: the asc and mf4 writers
// define their own rendering. A frame that does not Validate renders as a
// diagnostic struct instead, so an invalid frame is never mistaken for a record.
func (frame Frame) String() string {
	if err := frame.Validate(); err != nil {
		return fmt.Sprintf("Frame{ID: %#x, DLC: %d, Flags: %#x, Data: % X} (invalid)",
			frame.ID, frame.DLC, uint8(frame.Flags), frame.Data[:min(int(frame.DLC), MaxDataLength)])
	}

	buffer := make([]byte, 0, 8+2+2*MaxDataLength)
	if frame.Flags.Has(FrameExtended) {
		buffer = fmt.Appendf(buffer, "%08X#", frame.ID)
	} else {
		buffer = fmt.Appendf(buffer, "%03X#", frame.ID)
	}
	if frame.Flags.Has(FrameRemote) {
		buffer = append(buffer, 'R')
		if frame.DLC != 0 {
			buffer = append(buffer, hexDigits[frame.DLC])
		}
		return string(buffer)
	}
	if frame.Flags.Has(FrameFD) {
		var flags byte
		if frame.Flags.Has(FrameBitRateSwitch) {
			flags |= 1
		}
		if frame.Flags.Has(FrameErrorStateIndicator) {
			flags |= 2
		}
		buffer = append(buffer, '#', hexDigits[flags])
	}
	for _, value := range frame.Data[:frame.DataLength()] {
		buffer = append(buffer, hexDigits[value>>4], hexDigits[value&0x0f])
	}
	if !frame.Flags.Has(FrameFD) && frame.DLC > 8 {
		buffer = append(buffer, '_', hexDigits[frame.DLC])
	}
	return string(buffer)
}

// DLCToLength converts an on-wire data length code to a payload length in
// bytes.
func DLCToLength(dlc uint8, fd bool) (int, error) {
	if dlc > 15 {
		return 0, fmt.Errorf("DLC %d exceeds 15", dlc)
	}
	if dlc <= 8 {
		return int(dlc), nil
	}
	if !fd {
		return 8, nil
	}

	lengths := [...]int{12, 16, 20, 24, 32, 48, 64}
	return lengths[dlc-9], nil
}

// LengthToDLC converts a payload length in bytes to an on-wire data length
// code.
func LengthToDLC(length int, fd bool) (uint8, error) {
	if length < 0 {
		return 0, fmt.Errorf("%w: negative payload length %d", ErrInvalidFrame, length)
	}
	if length <= 8 {
		return uint8(length), nil
	}
	if !fd {
		return 0, fmt.Errorf("%w: classical CAN payload length %d exceeds 8", ErrInvalidFrame, length)
	}

	switch length {
	case 12:
		return 9, nil
	case 16:
		return 10, nil
	case 20:
		return 11, nil
	case 24:
		return 12, nil
	case 32:
		return 13, nil
	case 48:
		return 14, nil
	case 64:
		return 15, nil
	default:
		return 0, fmt.Errorf("%w: payload length %d has no CAN FD DLC", ErrInvalidFrame, length)
	}
}

// BusID is the one-based logical channel number of a bus in a Capture and
// exported trace files. Multiple buses may deliberately use the same ID to
// merge their records into one logical channel; the caller is responsible for
// avoiding unintended collisions. Records are never deduplicated. Zero is
// invalid.
type BusID uint16

// Direction describes whether a frame was received or transmitted.
type Direction uint8

const (
	// DirectionReceive indicates a frame received from a bus.
	DirectionReceive Direction = iota + 1
	// DirectionTransmit indicates a frame submitted for transmission.
	DirectionTransmit
)

// String returns the direction's name, or "Direction(n)" for an undefined value.
func (direction Direction) String() string {
	switch direction {
	case DirectionReceive:
		return "receive"
	case DirectionTransmit:
		return "transmit"
	default:
		return fmt.Sprintf("Direction(%d)", uint8(direction))
	}
}

// FrameKey identifies a frame series within a multi-bus capture.
//
// DLC, payload, and per-occurrence CAN FD flags are not part of the key.
// Direction keeps received traffic separate from accepted transmissions. The
// FD flag is a property of each frame, so consumers can use it to split mixed
// classical and CAN FD traffic within a series.
type FrameKey struct {
	ID        uint32
	Bus       BusID
	Direction Direction
	Extended  bool
}

// FrameEvent is one received or transmitted frame and its observation
// metadata.
type FrameEvent struct {
	Bus       BusID
	Timestamp time.Time
	Direction Direction
	Frame     Frame
}

// Key returns the stable series key for event.
func (event FrameEvent) Key() FrameKey {
	return FrameKey{
		Bus:       event.Bus,
		ID:        event.Frame.ID,
		Direction: event.Direction,
		Extended:  event.Frame.Flags.Has(FrameExtended),
	}
}

// Validate reports whether event is suitable for capture.
func (event FrameEvent) Validate() error {
	if event.Bus == 0 {
		return errors.New("frame event has no bus")
	}
	if event.Timestamp.IsZero() {
		return errors.New("frame event has no timestamp")
	}
	if event.Direction != DirectionReceive && event.Direction != DirectionTransmit {
		return fmt.Errorf("frame event has invalid direction %d", event.Direction)
	}
	return event.Frame.Validate()
}
