// Package zlg provides CAN access to ZLG USBCAN-I/II and USBCANFD-100U/200U/MINI
// adapters through ZLG's zlgcan.dll. Received frames use host timestamps;
// native timestamps only order frames read in one poll.
package zlg

import (
	"errors"
	"fmt"
	"slices"

	"github.com/tomrford/gocan"
)

type Config struct {
	ID   gocan.BusID
	Name string
	// DeviceType is the ZLG device type number reported by Discover.
	DeviceType uint32
	// Device is the zero-based ZLG device index within DeviceType.
	Device uint32
	// Channel is the zero-based CAN channel on the device.
	Channel uint32
	// Bitrate selects one of ZLG's predefined classical rates, or the nominal
	// rate when DataBitrate is set, in bits per second.
	Bitrate uint32
	// DataBitrate selects CAN FD on USBCANFD adapters using one of ZLG's
	// predefined data rates. Zero selects classical CAN.
	DataBitrate uint32
	// Termination is 0 to leave a USBCANFD adapter's terminator unchanged, 1 to
	// disable it, or 2 to enable it.
	Termination uint8
}

// zlgcan.dll maps ZCAN_USBCAN1 and ZCAN_USBCAN2 to the same kernel DLL, so
// deviceTypeUSBCAN2 selects every USBCAN-I/II family adapter. The USBCANFD
// types select USBCANFD.dll.
const (
	deviceTypeUSBCAN2      = 4
	deviceTypeUSBCANFD200U = 41
	deviceTypeUSBCANFD100U = 42
	deviceTypeUSBCANFDMini = 43
)

func isFDDevice(deviceType uint32) bool {
	switch deviceType {
	case deviceTypeUSBCANFD200U, deviceTypeUSBCANFD100U, deviceTypeUSBCANFDMini:
		return true
	default:
		return false
	}
}

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

// USBCANFD.dll's canfd_abit_baud_rate and canfd_dbit_baud_rate choices for its
// 60 MHz clock, from ZLG's devices_property/usbcanfd-200u.xml. Every rate
// samples at 80% except 5 Mbit/s data at 75%.
var (
	usbcanfdNominalRates = []uint32{50_000, 100_000, 125_000, 250_000, 500_000, 800_000, 1_000_000}
	usbcanfdDataRates    = []uint32{100_000, 125_000, 250_000, 500_000, 800_000, 1_000_000, 2_000_000, 4_000_000, 5_000_000}
)

func validateConfig(config Config) error {
	if !isFDDevice(config.DeviceType) {
		if config.DeviceType != deviceTypeUSBCAN2 {
			return fmt.Errorf("unsupported ZLG device type %d", config.DeviceType)
		}
		switch {
		case config.DataBitrate != 0:
			return errors.New("ZLG USBCAN adapters do not support CAN FD")
		case config.Termination != 0:
			return errors.New("ZLG USBCAN adapters have no switchable termination")
		}
		if _, ok := sja1000Timings[config.Bitrate]; !ok {
			return fmt.Errorf("ZLG classical bitrate %d is not a ZLG predefined rate", config.Bitrate)
		}
		return nil
	}
	switch {
	case !slices.Contains(usbcanfdNominalRates, config.Bitrate):
		return fmt.Errorf("ZLG USBCANFD bitrate %d is not a ZLG predefined rate", config.Bitrate)
	case config.DataBitrate != 0 && (!slices.Contains(usbcanfdDataRates, config.DataBitrate) || config.DataBitrate < config.Bitrate):
		return fmt.Errorf("ZLG USBCANFD data bitrate %d is not a ZLG predefined rate at or above %d", config.DataBitrate, config.Bitrate)
	case config.Termination > 2:
		return fmt.Errorf("invalid ZLG termination choice %d", config.Termination)
	}
	return nil
}

const (
	statusOK = 1

	canTypeFD = 1

	canExtendedFlag = 0x80000000
	canRemoteFlag   = 0x40000000
	canErrorFlag    = 0x20000000

	canFDBitRateSwitch = 0x01
	canFDErrorState    = 0x02

	errorControllerOverflow = 0x0001
	errorWarning            = 0x0002
	errorPassive            = 0x0004
	errorBusOff             = 0x0020
	errorBufferOverflow     = 0x0040
	errorDeviceOverflow     = 0x0800

	sja1000BusStatus = 0x80
)

// The structs mirror zlgcan.h and canframe.h. ZLG compiles canfd_frame without
// Linux's 8-byte data alignment, so transmitFDData is 76 bytes.
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

// USBCANFD.dll takes its timing from the canfd_*_baud_rate values, so the
// timing and prescaler fields stay zero.
type initFDConfig struct {
	canType    uint32
	accCode    uint32
	accMask    uint32
	abitTiming uint32
	dbitTiming uint32
	brp        uint32
	filter     uint8
	mode       uint8
	_          uint16
	reserved   uint32
}

type canFrame struct {
	id   uint32
	dlc  uint8
	_    [3]uint8
	data [8]byte
}

type canFDFrame struct {
	id     uint32
	length uint8
	flags  uint8
	_      [2]uint8
	data   [64]byte
}

type transmitData struct {
	frame        canFrame
	transmitType uint32
}

type receiveData struct {
	frame     canFrame
	timestamp uint64
}

type transmitFDData struct {
	frame        canFDFrame
	transmitType uint32
}

type receiveFDData struct {
	frame     canFDFrame
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

func validateSendFrame(frame gocan.Frame, fd bool) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.Flags.Has(gocan.FrameFD) {
		switch {
		case !fd:
			return errors.New("ZLG classical bus cannot send a CAN FD frame")
		case frame.Flags.Has(gocan.FrameErrorStateIndicator):
			return errors.New("ZLG cannot transmit the error state indicator")
		}
		return nil
	}
	if frame.DLC > 8 {
		return fmt.Errorf("ZLG classical API cannot send DLC %d", frame.DLC)
	}
	return nil
}

func encodeID(frame gocan.Frame) uint32 {
	id := frame.ID
	if frame.Flags.Has(gocan.FrameExtended) {
		id |= canExtendedFlag
	}
	if frame.Flags.Has(gocan.FrameRemote) {
		id |= canRemoteFlag
	}
	return id
}

func encodeFrame(frame gocan.Frame) canFrame {
	native := canFrame{id: encodeID(frame), dlc: frame.DLC}
	copy(native.data[:], frame.Data[:frame.DataLength()])
	return native
}

func encodeFDFrame(frame gocan.Frame) canFDFrame {
	length := frame.DataLength()
	native := canFDFrame{id: encodeID(frame), length: uint8(length)}
	if frame.Flags.Has(gocan.FrameBitRateSwitch) {
		native.flags |= canFDBitRateSwitch
	}
	copy(native.data[:], frame.Data[:length])
	return native
}

func decodeID(id uint32) (gocan.Frame, error) {
	if id&canErrorFlag != 0 {
		return gocan.Frame{}, fmt.Errorf("unsupported ZLG error frame %#08x", id)
	}
	frame := gocan.Frame{ID: id &^ (canExtendedFlag | canRemoteFlag)}
	if id&canExtendedFlag != 0 {
		frame.Flags |= gocan.FrameExtended
	}
	if id&canRemoteFlag != 0 {
		frame.Flags |= gocan.FrameRemote
	}
	return frame, nil
}

// decodeFrame ignores data beyond the DLC: the adapter leaves stale bytes
// there, including in remote frames.
func decodeFrame(native canFrame) (gocan.Frame, error) {
	frame, err := decodeID(native.id)
	if err != nil {
		return gocan.Frame{}, err
	}
	frame.DLC = native.dlc
	if !frame.Flags.Has(gocan.FrameRemote) && native.dlc <= 8 {
		copy(frame.Data[:], native.data[:native.dlc])
	}
	if err := frame.Validate(); err != nil {
		return gocan.Frame{}, fmt.Errorf("decode ZLG frame: %w", err)
	}
	return frame, nil
}

func decodeFDFrame(native canFDFrame) (gocan.Frame, error) {
	frame, err := decodeID(native.id)
	if err != nil {
		return gocan.Frame{}, err
	}
	frame.Flags |= gocan.FrameFD
	if native.flags&canFDBitRateSwitch != 0 {
		frame.Flags |= gocan.FrameBitRateSwitch
	}
	if native.flags&canFDErrorState != 0 {
		frame.Flags |= gocan.FrameErrorStateIndicator
	}
	if frame.DLC, err = gocan.LengthToDLC(int(native.length), true); err != nil {
		return gocan.Frame{}, fmt.Errorf("decode ZLG CAN FD frame: %w", err)
	}
	copy(frame.Data[:], native.data[:frame.DataLength()])
	if err := frame.Validate(); err != nil {
		return gocan.Frame{}, fmt.Errorf("decode ZLG CAN FD frame: %w", err)
	}
	return frame, nil
}

// controllerEvent maps a polled controller status and the accumulated error
// code, which reading clears, so terminal conditions are reported once. Only
// SJA1000-based USBCAN adapters report a bus status bit. A nil status leaves
// the error counters unknown and takes the state from the error code.
func controllerEvent(bus gocan.BusID, status *channelStatus, errorCode uint32, sja1000 bool) (gocan.Event, error) {
	if errorCode&(errorControllerOverflow|errorBufferOverflow|errorDeviceOverflow) != 0 {
		return gocan.Event{Bus: bus, Kind: gocan.EventReceiveOverrun},
			fmt.Errorf("%w: ZLG error code %#04x", gocan.ErrReceiveOverrun, errorCode)
	}
	event := gocan.Event{Bus: bus, Kind: gocan.EventControllerState}
	passive, warning := errorCode&errorPassive != 0, errorCode&errorWarning != 0
	if status != nil {
		event.TXErrorCount, event.RXErrorCount, event.ErrorCountsKnown = status.txErrorCount, status.rxErrorCount, true
		passive = status.txErrorCount >= 128 || status.rxErrorCount >= 128
		warning = status.txErrorCount >= 96 || status.rxErrorCount >= 96
	}
	switch {
	case errorCode&errorBusOff != 0 || sja1000 && status != nil && status.status&sja1000BusStatus != 0:
		event.ControllerState = gocan.ControllerBusOff
		return event, fmt.Errorf("%w: ZLG controller entered bus-off", gocan.ErrBusOff)
	case passive:
		event.ControllerState = gocan.ControllerPassive
	case warning:
		event.ControllerState = gocan.ControllerWarning
	default:
		event.ControllerState = gocan.ControllerActive
	}
	return event, nil
}
