// Package zlg provides classical CAN access to ZLG USBCAN-I/II family
// adapters through ZLG's zlgcan.dll. Received frames use host timestamps;
// native timestamps are not read.
package zlg

import (
	"errors"
	"fmt"

	"github.com/tomrford/gocan"
)

type Config struct {
	ID   gocan.BusID
	Name string
	// Device is the zero-based ZLG device index.
	Device uint32
	// Channel is the zero-based CAN channel on the device.
	Channel uint32
	// Bitrate selects one of ZLG's predefined classical rates in bits per second.
	Bitrate uint32
}

// deviceTypeUSBCAN2 selects USBCAN.dll for every USBCAN-I/II family adapter;
// zlgcan.dll maps ZCAN_USBCAN1 and ZCAN_USBCAN2 to the same kernel DLL.
const deviceTypeUSBCAN2 = 4

// sja1000Timings maps a bitrate to ZLG's documented SJA1000 BTR0/BTR1 values
// for the USBCAN family's 16 MHz controller clock.
var sja1000Timings = map[uint32][2]uint8{
	5_000:     {0xbf, 0xff},
	10_000:    {0x31, 0x1c},
	20_000:    {0x18, 0x1c},
	40_000:    {0x87, 0xff},
	50_000:    {0x09, 0x1c},
	80_000:    {0x83, 0xff},
	100_000:   {0x04, 0x1c},
	125_000:   {0x03, 0x1c},
	200_000:   {0x81, 0xfa},
	250_000:   {0x01, 0x1c},
	400_000:   {0x80, 0xfa},
	500_000:   {0x00, 0x1c},
	800_000:   {0x00, 0x16},
	1_000_000: {0x00, 0x14},
}

const (
	statusOK = 1

	canExtendedFlag = 0x80000000
	canRemoteFlag   = 0x40000000
	canErrorFlag    = 0x20000000

	errorControllerOverflow = 0x0001
	errorBusOff             = 0x0020
	errorBufferOverflow     = 0x0040
	errorDeviceOverflow     = 0x0800

	sja1000BusStatus = 0x80
)

// The structs mirror zlgcan.h, whose CAN structures are naturally aligned.
type initConfig struct {
	canType  uint32
	accCode  uint32
	accMask  uint32
	reserved uint32
	filter   uint8
	timing0  uint8
	timing1  uint8
	mode     uint8
	_        [12]byte // the union's CAN FD member is 28 bytes
}

type canFrame struct {
	id   uint32
	dlc  uint8
	_    [3]uint8
	data [8]byte
}

type transmitData struct {
	frame        canFrame
	transmitType uint32
}

type receiveData struct {
	frame     canFrame
	timestamp uint64
}

type deviceInfo struct {
	hardwareVersion uint16
	firmwareVersion uint16
	driverVersion   uint16
	libraryVersion  uint16
	irq             uint16
	channels        uint8
	serial          [20]byte
	hardwareType    [40]byte
	reserved        [4]uint16
}

type channelErrorInfo struct {
	code            uint32
	passive         [3]byte
	arbitrationLost byte
}

type channelStatus struct {
	errorInterrupt uint8
	mode           uint8
	status         uint8
	arbitration    uint8
	errorCapture   uint8
	warningLimit   uint8
	rxErrorCount   uint8
	txErrorCount   uint8
	reserved       uint32
}

func validateSendFrame(frame gocan.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.Flags.Has(gocan.FrameFD) {
		return errors.New("ZLG classical bus cannot send a CAN FD frame")
	}
	if frame.DLC > 8 {
		return fmt.Errorf("ZLG classical API cannot send DLC %d", frame.DLC)
	}
	return nil
}

func encodeFrame(frame gocan.Frame) canFrame {
	native := canFrame{id: frame.ID, dlc: frame.DLC}
	if frame.Flags.Has(gocan.FrameExtended) {
		native.id |= canExtendedFlag
	}
	if frame.Flags.Has(gocan.FrameRemote) {
		native.id |= canRemoteFlag
	} else {
		copy(native.data[:], frame.Data[:frame.DataLength()])
	}
	return native
}

// decodeFrame ignores data beyond the DLC: the adapter leaves stale bytes
// there, including in remote frames.
func decodeFrame(native canFrame) (gocan.Frame, error) {
	if native.id&canErrorFlag != 0 {
		return gocan.Frame{}, fmt.Errorf("unsupported ZLG error frame %#08x", native.id)
	}
	frame := gocan.Frame{ID: native.id &^ (canExtendedFlag | canRemoteFlag), DLC: native.dlc}
	if native.id&canExtendedFlag != 0 {
		frame.Flags |= gocan.FrameExtended
	}
	if native.id&canRemoteFlag != 0 {
		frame.Flags |= gocan.FrameRemote
	} else if native.dlc <= 8 {
		copy(frame.Data[:], native.data[:native.dlc])
	}
	if err := frame.Validate(); err != nil {
		return gocan.Frame{}, fmt.Errorf("decode ZLG frame: %w", err)
	}
	return frame, nil
}

// controllerEvent maps a polled controller status and the accumulated error
// code. The error code is cleared by reading, so terminal conditions are
// reported once.
func controllerEvent(bus gocan.BusID, status channelStatus, errorCode uint32) (gocan.Event, error) {
	if errorCode&(errorControllerOverflow|errorBufferOverflow|errorDeviceOverflow) != 0 {
		return gocan.Event{Bus: bus, Kind: gocan.EventReceiveOverrun},
			fmt.Errorf("%w: ZLG error code %#04x", gocan.ErrReceiveOverrun, errorCode)
	}
	event := gocan.Event{
		Bus:              bus,
		Kind:             gocan.EventControllerState,
		TXErrorCount:     status.txErrorCount,
		RXErrorCount:     status.rxErrorCount,
		ErrorCountsKnown: true,
	}
	switch {
	case errorCode&errorBusOff != 0 || status.status&sja1000BusStatus != 0:
		event.ControllerState = gocan.ControllerBusOff
		return event, fmt.Errorf("%w: ZLG controller entered bus-off", gocan.ErrBusOff)
	case status.txErrorCount >= 128 || status.rxErrorCount >= 128:
		event.ControllerState = gocan.ControllerPassive
	case status.txErrorCount >= 96 || status.rxErrorCount >= 96:
		event.ControllerState = gocan.ControllerWarning
	default:
		event.ControllerState = gocan.ControllerActive
	}
	return event, nil
}
