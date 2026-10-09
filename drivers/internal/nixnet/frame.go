// Package nixnet implements NI-XNET frame streams. Timing and timestamps are
// owned by Go; the native stream transmits each submitted frame immediately.
// Fixed high-speed transceivers support FD; XS transceiver selection is not
// configured. NI raw CAN frames do not expose ESI or classical DLC above 8.
package nixnet

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/tomrford/gocan"
)

// Config is prepared by package drivers; Baud and FDBaud may be NI's encoded
// 64-bit timing values. Termination is 0 (default), 1 (off), or 2 (on).
type Config struct {
	ID              gocan.BusID
	Name, Interface string
	Baud, FDBaud    uint64
	FD              bool
	Termination     uint8
	ConcurrentIO    bool
}

const (
	frameHeader      = 16
	frameClassicSize = 24
	frameMaxSize     = 80
	frameData        = 0x00
	frameRemote      = 0x01
	frameError       = 0x02
	frameCAN20       = 0x08
	frameFD          = 0x10
	frameFDBRS       = 0x18
	extendedID       = 0x20000000
	echoFlag         = 0x80
)

func recordSize(length int) int {
	if length < 8 {
		length = 8
	}
	return frameHeader + ((length + 7) &^ 7)
}

func encodeFrame(frame gocan.Frame, fd bool) ([frameMaxSize]byte, int, error) {
	var data [frameMaxSize]byte
	if err := frame.Validate(); err != nil {
		return data, 0, err
	}
	if frame.Flags.Has(gocan.FrameFD) && !fd {
		return data, 0, errors.New("NI-XNET classical bus cannot send CAN FD")
	}
	if !frame.Flags.Has(gocan.FrameFD) && frame.DLC > 8 {
		return data, 0, errors.New("NI-XNET cannot preserve classical DLC above 8")
	}
	// NI's documented CAN raw format has no ESI field. Never silently discard
	// a caller's requested ESI; the controller owns the on-wire error state.
	if frame.Flags.Has(gocan.FrameErrorStateIndicator) {
		return data, 0, errors.New("NI-XNET raw frames cannot specify ESI")
	}
	id := frame.ID
	if frame.Flags.Has(gocan.FrameExtended) {
		id |= extendedID
	}
	binary.LittleEndian.PutUint32(data[8:], id)
	length := frame.DataLength()
	switch {
	case frame.Flags.Has(gocan.FrameRemote):
		data[12] = frameRemote
		length = int(frame.DLC)
	case frame.Flags.Has(gocan.FrameBitRateSwitch):
		data[12] = frameFDBRS
	case frame.Flags.Has(gocan.FrameFD):
		data[12] = frameFD
	case fd:
		data[12] = frameCAN20
	default:
		data[12] = frameData
	}
	data[15] = byte(length)
	copy(data[16:], frame.Data[:frame.DataLength()])
	return data, recordSize(length), nil
}

func decodeFrame(data []byte) (gocan.Frame, error) {
	var frame gocan.Frame
	if len(data) < frameClassicSize {
		return frame, errors.New("short NI-XNET frame")
	}
	id := binary.LittleEndian.Uint32(data[8:])
	frame.ID = id &^ uint32(extendedID)
	if id&extendedID != 0 {
		frame.Flags |= gocan.FrameExtended
	}
	switch data[12] {
	case frameData, frameCAN20:
	case frameRemote:
		frame.Flags |= gocan.FrameRemote
	case frameFD:
		frame.Flags |= gocan.FrameFD
	case frameFDBRS:
		frame.Flags |= gocan.FrameFD | gocan.FrameBitRateSwitch
	default:
		return frame, fmt.Errorf("unsupported NI-XNET frame type %#x", data[12])
	}
	length := int(data[15])
	if length > 64 || len(data) < recordSize(length) {
		return frame, errors.New("invalid NI-XNET payload size")
	}
	dlc, err := gocan.LengthToDLC(length, frame.Flags.Has(gocan.FrameFD))
	if err != nil {
		return frame, err
	}
	frame.DLC = dlc
	if !frame.Flags.Has(gocan.FrameRemote) {
		copy(frame.Data[:], data[16:16+length])
	}
	return frame, frame.Validate()
}

func controllerEvent(bus gocan.BusID, state, tx, rx byte) (gocan.Event, error) {
	event := gocan.Event{Bus: bus, Kind: gocan.EventControllerState, TXErrorCount: tx, RXErrorCount: rx, ErrorCountsKnown: true}
	switch state {
	case 0:
		event.ControllerState = gocan.ControllerActive
		if tx >= 96 || rx >= 96 {
			event.ControllerState = gocan.ControllerWarning
		}
	case 1:
		event.ControllerState = gocan.ControllerPassive
	case 2:
		event.ControllerState = gocan.ControllerBusOff
		event.ErrorCountsKnown = false
		event.TXErrorCount = 0
		event.RXErrorCount = 0
	default:
		return gocan.Event{}, fmt.Errorf("unexpected NI-XNET controller state %d", state)
	}
	return event, nil
}
