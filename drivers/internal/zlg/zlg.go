// Package zlg provides CAN access to ZLG USBCAN-I/II and USBCANFD-100U/200U/MINI
// adapters through ZLG's zlgcan.dll. Received frames use host timestamps;
// native timestamps only order frames read in one poll.
package zlg

import (
	"errors"
	"fmt"
	"slices"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/drivercfg"
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
	// Termination selects switchable termination on USBCANFD adapters.
	Termination drivercfg.Termination
}

// FDRate is a nominal/data bitrate pair in bits per second supported by ZLG
// USBCANFD built-in timing.
type FDRate struct {
	Bitrate     uint32
	DataBitrate uint32
}

// FDRates are rate pairs that ZLG USBCANFD adapters configure from their own
// built-in timing, sampling at 80% except at 5 Mbit/s data (75%).
var FDRates = [...]FDRate{
	{Bitrate: 500_000, DataBitrate: 2_000_000},
	{Bitrate: 500_000, DataBitrate: 4_000_000},
	{Bitrate: 500_000, DataBitrate: 5_000_000},
	{Bitrate: 1_000_000, DataBitrate: 2_000_000},
	{Bitrate: 1_000_000, DataBitrate: 4_000_000},
	{Bitrate: 1_000_000, DataBitrate: 5_000_000},
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

// USBCANFD.dll's canfd_abit_baud_rate choices for its 60 MHz clock, from ZLG's
// devices_property/usbcanfd-200u.xml.
var usbcanfdNominalRates = []uint32{50_000, 100_000, 125_000, 250_000, 500_000, 800_000, 1_000_000}

// ValidateConfig checks ZLG device type, termination, and predefined bitrates.
func ValidateConfig(config Config) error {
	fdDevice := isFDDevice(config.DeviceType)
	if !fdDevice && config.DeviceType != deviceTypeUSBCAN2 {
		return fmt.Errorf("%w: unsupported ZLG device type %d", drivercfg.ErrInvalidConfig, config.DeviceType)
	}
	if !config.Termination.Valid() {
		return fmt.Errorf("%w: invalid ZLG termination choice %d", drivercfg.ErrInvalidConfig, config.Termination)
	}
	if !fdDevice {
		switch {
		case config.DataBitrate != 0:
			return fmt.Errorf("%w: ZLG USBCAN adapters do not support CAN FD", drivercfg.ErrInvalidConfig)
		case config.Termination != drivercfg.TerminationDefault:
			return fmt.Errorf("%w: ZLG USBCAN adapters have no switchable termination", drivercfg.ErrInvalidConfig)
		}
		if _, ok := sja1000Timings[config.Bitrate]; !ok {
			return fmt.Errorf("%w: ZLG classical bitrate %d is not a ZLG predefined rate", drivercfg.ErrInvalidConfig, config.Bitrate)
		}
		return nil
	}
	if config.DataBitrate != 0 {
		if !slices.Contains(FDRates[:], FDRate{Bitrate: config.Bitrate, DataBitrate: config.DataBitrate}) {
			return fmt.Errorf("%w: ZLG USBCANFD rate pair %d/%d is not a ZLG predefined rate", drivercfg.ErrInvalidConfig, config.Bitrate, config.DataBitrate)
		}
		return nil
	}
	if !slices.Contains(usbcanfdNominalRates, config.Bitrate) {
		return fmt.Errorf("%w: ZLG USBCANFD bitrate %d is not a ZLG predefined rate", drivercfg.ErrInvalidConfig, config.Bitrate)
	}
	return nil
}

const (
	statusOK = 1

	// ZCAN_CHANNEL_INIT_CONFIG.can_type and ZCAN_GetReceiveNum's frame type.
	typeCAN   = 0
	typeCANFD = 1

	canExtendedFlag = 0x80000000
	canRemoteFlag   = 0x40000000
	canErrorFlag    = 0x20000000

	canFDBitRateSwitch = 0x01
	canFDErrorState    = 0x02

	errorControllerOverflow = 0x0001
	errorAlarm              = 0x0002
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

// errErrorFrame marks a received frame carrying ZLG's error flag.
var errErrorFrame = errors.New("ZLG error frame")

func decodeID(id uint32) (gocan.Frame, error) {
	if id&canErrorFlag != 0 {
		return gocan.Frame{}, errErrorFrame
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
	copy(frame.Data[:], native.data[:frame.DataLength()])
	if err := frame.Validate(); err != nil {
		return gocan.Frame{}, fmt.Errorf("decode ZLG frame (id %#08x, DLC %d, data % x): %w", native.id, native.dlc, native.data, err)
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
// code, which reading clears, so a loss is reported once. Only SJA1000-based
// USBCAN adapters report a bus status bit. Without a status (USBCANFD) the
// error counters are unknown: bus-off, passive, and alarm bits map to their
// controller states, and a subsequent clean zero poll after any degraded state
// infers ControllerActive with unknown counts; otherwise the returned event has
// no Kind.
func controllerEvent(bus gocan.BusID, status *channelStatus, errorCode uint32, sja1000, wasDegraded bool) (state gocan.Event, lost bool) {
	lost = errorCode&(errorControllerOverflow|errorBufferOverflow|errorDeviceOverflow) != 0
	event := gocan.Event{Bus: bus, Kind: gocan.EventControllerState}
	if status != nil {
		event.TXErrorCount, event.RXErrorCount, event.ErrorCountsKnown = status.txErrorCount, status.rxErrorCount, true
	}
	switch {
	case errorCode&errorBusOff != 0 || sja1000 && status != nil && status.status&sja1000BusStatus != 0:
		event.ControllerState = gocan.ControllerBusOff
	case status == nil:
		switch {
		case errorCode&errorPassive != 0:
			event.ControllerState = gocan.ControllerPassive
		case errorCode&errorAlarm != 0:
			event.ControllerState = gocan.ControllerWarning
		case errorCode == 0 && wasDegraded:
			event.ControllerState = gocan.ControllerActive
		default:
			return gocan.Event{}, lost
		}
	case status.txErrorCount >= 128 || status.rxErrorCount >= 128:
		event.ControllerState = gocan.ControllerPassive
	case status.txErrorCount >= 96 || status.rxErrorCount >= 96:
		event.ControllerState = gocan.ControllerWarning
	default:
		event.ControllerState = gocan.ControllerActive
	}
	return event, lost
}
