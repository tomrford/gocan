//go:build linux

// Package socketcan provides CAN access through Linux SocketCAN interfaces.
//
// Received frames use host timestamps; kernel timestamps are not read.
package socketcan

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/internal/driverstate"
	"golang.org/x/sys/unix"
)

const (
	classicalMTU = 16
	fdMTU        = 72

	canFDBitRateSwitch = 0x01
	canFDErrorState    = 0x02
	canFDFlexibleData  = 0x04

	// State, recovery, counter-validity, and arbitration notifications are not
	// error observations by themselves.
	canErrorObservationMask = unix.CAN_ERR_TX_TIMEOUT |
		unix.CAN_ERR_PROT |
		unix.CAN_ERR_TRX |
		unix.CAN_ERR_ACK |
		unix.CAN_ERR_BUSERROR
)

// Config selects one Linux SocketCAN interface. Package drivers constructs
// and validates it.
type Config struct {
	// ID is the one-based trace channel assigned to the bus.
	ID gocan.BusID
	// Name is the human-readable name of the bus.
	Name string
	// Interface is the Linux network interface name, such as can0 or vcan0.
	Interface string
	// FD enables mixed classical CAN and CAN FD frames on the socket.
	FD bool
}

// Open opens a SocketCAN interface and starts capturing received frames.
// Context controls opening only; canceling it after Open returns does not stop
// the bus.
func Open(ctx context.Context, capture *gocan.Capture, config Config) (*Bus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	iface, err := net.InterfaceByName(config.Interface)
	if err != nil {
		return nil, fmt.Errorf("find SocketCAN interface %q: %w", config.Interface, err)
	}

	fd, err := unix.Socket(
		unix.AF_CAN,
		unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK,
		unix.CAN_RAW,
	)
	if err != nil {
		return nil, fmt.Errorf("open SocketCAN interface %q: %w", config.Interface, err)
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()

	if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_LOOPBACK, 1); err != nil {
		return nil, fmt.Errorf("enable SocketCAN loopback: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_RECV_OWN_MSGS, 0); err != nil {
		return nil, fmt.Errorf("disable SocketCAN own-message reception: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_ERR_FILTER, unix.CAN_ERR_MASK); err != nil {
		return nil, fmt.Errorf("enable SocketCAN error frames: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RXQ_OVFL, 1); err != nil {
		return nil, fmt.Errorf("enable SocketCAN receive-overrun reporting: %w", err)
	}
	if config.FD {
		if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_FD_FRAMES, 1); err != nil {
			return nil, fmt.Errorf("enable SocketCAN FD frames: %w", err)
		}
	}
	if err := unix.Bind(fd, &unix.SockaddrCAN{Ifindex: iface.Index}); err != nil {
		return nil, fmt.Errorf("bind SocketCAN interface %q: %w", config.Interface, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	file := os.NewFile(uintptr(fd), "socketcan:"+config.Interface)
	if file == nil {
		return nil, errors.New("create SocketCAN file")
	}
	closeFD = false

	raw, err := file.SyscallConn()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("access SocketCAN file: %w", err)
	}

	bus := &Bus{
		id:      config.ID,
		name:    config.Name,
		capture: capture,
		file:    file,
		raw:     raw,
		lifecycle: driverstate.New(func() {
			_ = file.Close()
		}),
	}
	bus.rxIovec.Base = &bus.rxPacket[0]
	bus.rxIovec.SetLen(len(bus.rxPacket))
	bus.rxMessage.Iov = &bus.rxIovec
	bus.rxMessage.SetIovlen(1)
	bus.rxMessage.Control = &bus.rxControl[0]
	bus.receiveReady = bus.receiveOnce
	bus.sendReady = bus.sendOnce
	go bus.receiveLoop()
	return bus, nil
}

// Bus is one open Linux SocketCAN interface.
type Bus struct {
	id      gocan.BusID
	name    string
	capture *gocan.Capture
	file    *os.File
	raw     syscall.RawConn

	sendMu    sync.Mutex
	sendReady func(uintptr)
	txPacket  [fdMTU]byte
	txSize    int
	txSent    int
	txErr     error

	receiveReady  func(uintptr) bool
	rxPacket      [fdMTU]byte
	rxControl     [32]byte
	rxIovec       unix.Iovec
	rxMessage     unix.Msghdr
	rxSize        int
	rxControlSize int
	rxFlags       int
	rxErr         error

	lifecycle *driverstate.Lifecycle
}

var _ gocan.Bus = (*Bus)(nil)

// ID returns the one-based trace channel assigned to this bus.
func (bus *Bus) ID() gocan.BusID {
	return bus.id
}

// Name returns the human-readable name of this bus.
func (bus *Bus) Name() string {
	return bus.name
}

// Send hands frame to the Linux CAN socket and records the accepted
// transmission before returning.
func (bus *Bus) Send(ctx context.Context, frame gocan.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bus.lifecycle.StopSignal():
		return bus.operationError()
	default:
	}

	bus.sendMu.Lock()
	defer bus.sendMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bus.lifecycle.StopSignal():
		return bus.operationError()
	default:
	}

	packet := encodeFrame(frame, &bus.txPacket)
	bus.txSize = len(packet)
	bus.txSent = 0
	bus.txErr = nil
	if err := bus.raw.Control(bus.sendReady); err != nil {
		select {
		case <-bus.lifecycle.StopSignal():
			return bus.operationError()
		default:
			return fmt.Errorf("access SocketCAN socket for send: %w", err)
		}
	}
	if bus.txErr != nil {
		select {
		case <-bus.lifecycle.StopSignal():
			return bus.operationError()
		default:
			if errors.Is(bus.txErr, unix.ENOBUFS) {
				return fmt.Errorf("%w: SocketCAN send returned %v", gocan.ErrTransmitQueueFull, bus.txErr)
			}
			return fmt.Errorf("send SocketCAN frame: %w", bus.txErr)
		}
	}
	if bus.txSent != len(packet) {
		return fmt.Errorf("send SocketCAN frame: wrote %d of %d bytes", bus.txSent, len(packet))
	}

	event := gocan.FrameEvent{
		Bus:       bus.id,
		Timestamp: time.Now(),
		Direction: gocan.DirectionTransmit,
		Frame:     frame,
	}
	if err := bus.capture.Append(event); err != nil {
		bus.stopWithError(err)
		return err
	}
	return nil
}

// Done is closed when this bus stops.
func (bus *Bus) Done() <-chan struct{} {
	return bus.lifecycle.Done()
}

// Err returns the background failure that stopped this bus, if any.
func (bus *Bus) Err() error {
	return bus.lifecycle.Err()
}

// Close stops acquisition. It is safe to call more than once.
func (bus *Bus) Close() error {
	bus.sendMu.Lock()
	bus.stopWithError(nil)
	bus.sendMu.Unlock()
	<-bus.lifecycle.Done()
	return nil
}

// receiveLoop reads one frame per kernel round trip and appends it.
func (bus *Bus) receiveLoop() {
	defer bus.lifecycle.MarkDone()
	for {
		packet, timestamp, err := bus.receive()
		if err != nil {
			terminal := fmt.Errorf("receive SocketCAN frame: %w", err)
			if errors.Is(err, gocan.ErrReceiveOverrun) {
				event, eventErr := gocan.NewReceiveOverrunEvent(bus.id, timestamp)
				if eventErr != nil {
					bus.stopWithError(eventErr)
					return
				}
				bus.stopWithEvents(terminal, []gocan.Event{event})
				return
			}
			select {
			case <-bus.lifecycle.StopSignal():
				return
			default:
				bus.stopWithError(terminal)
				return
			}
		}
		if len(packet) >= 4 && binary.NativeEndian.Uint32(packet[:4])&unix.CAN_ERR_FLAG != 0 {
			errorPacket, err := decodeErrorPacket(packet, bus.id, timestamp)
			if err != nil {
				bus.stopWithError(err)
				return
			}
			if errorPacket.terminal != nil {
				bus.stopWithEvents(
					errorPacket.terminal,
					errorPacket.events[:errorPacket.eventCount],
				)
				return
			}
			for index := range errorPacket.eventCount {
				if err := bus.capture.AppendEvent(errorPacket.events[index]); err != nil {
					bus.stopWithError(err)
					return
				}
			}
			continue
		}

		frame, err := decodeFrame(packet)
		if err != nil {
			bus.stopWithError(err)
			return
		}
		if err := bus.capture.Append(gocan.FrameEvent{
			Bus:       bus.id,
			Timestamp: timestamp,
			Direction: gocan.DirectionReceive,
			Frame:     frame,
		}); err != nil {
			bus.stopWithError(err)
			return
		}
	}
}

func (bus *Bus) receive() ([]byte, time.Time, error) {
	bus.rxSize = 0
	bus.rxControlSize = 0
	bus.rxFlags = 0
	bus.rxErr = nil
	err := bus.raw.Read(bus.receiveReady)
	timestamp := time.Now()
	if err != nil {
		return nil, time.Time{}, err
	}
	if bus.rxErr != nil {
		return nil, time.Time{}, bus.rxErr
	}
	if bus.rxFlags&unix.MSG_CTRUNC != 0 {
		return nil, timestamp, fmt.Errorf("%w: SocketCAN receive metadata was truncated", gocan.ErrReceiveOverrun)
	}
	if bus.rxControlSize != 0 {
		dropped, err := socketReceiveDropCount(bus.rxControl[:bus.rxControlSize])
		if err != nil {
			return nil, timestamp, err
		}
		if dropped != 0 {
			return nil, timestamp, fmt.Errorf("%w: SocketCAN socket dropped %d frames", gocan.ErrReceiveOverrun, dropped)
		}
	}
	return bus.rxPacket[:bus.rxSize], timestamp, nil
}

func (bus *Bus) receiveOnce(fd uintptr) bool {
	bus.rxMessage.SetIovlen(1)
	bus.rxMessage.SetControllen(len(bus.rxControl))
	bus.rxMessage.Flags = 0

	read, _, errno := unix.Syscall(
		unix.SYS_RECVMSG,
		fd,
		uintptr(unsafe.Pointer(&bus.rxMessage)),
		unix.MSG_DONTWAIT,
	)
	bus.rxSize = int(read)
	bus.rxControlSize = int(bus.rxMessage.Controllen)
	bus.rxFlags = int(bus.rxMessage.Flags)
	if errno != 0 {
		bus.rxErr = errno
	} else {
		bus.rxErr = nil
	}
	return bus.rxErr != unix.EAGAIN && bus.rxErr != unix.EWOULDBLOCK
}

func socketReceiveDropCount(control []byte) (uint32, error) {
	messages, err := unix.ParseSocketControlMessage(control)
	if err != nil {
		return 0, fmt.Errorf("parse SocketCAN receive metadata: %w", err)
	}
	for _, message := range messages {
		if message.Header.Level != unix.SOL_SOCKET || message.Header.Type != unix.SO_RXQ_OVFL {
			continue
		}
		if len(message.Data) < 4 {
			return 0, fmt.Errorf("SocketCAN receive-overrun metadata has length %d", len(message.Data))
		}
		return binary.NativeEndian.Uint32(message.Data[:4]), nil
	}
	return 0, nil
}

// sendOnce writes without blocking, so a full transmit queue surfaces
// ENOBUFS to the caller immediately; vcan never fills, real interfaces do.
func (bus *Bus) sendOnce(fd uintptr) {
	bus.txSent, bus.txErr = unix.SendmsgN(
		int(fd),
		bus.txPacket[:bus.txSize],
		nil,
		nil,
		unix.MSG_DONTWAIT,
	)
}

func (bus *Bus) stopWithError(err error) {
	bus.lifecycle.Stop(err)
}

// stopWithEvents serializes a terminal transition with Send. A native send
// already in progress completes before the terminal events; later sends see
// the stopped bus. Done closes only after this receive loop returns.
func (bus *Bus) stopWithEvents(terminal error, events []gocan.Event) {
	bus.sendMu.Lock()
	defer bus.sendMu.Unlock()

	for _, event := range events {
		if err := bus.capture.AppendEvent(event); err != nil {
			bus.stopWithError(err)
			return
		}
	}
	bus.stopWithError(terminal)
}

func (bus *Bus) operationError() error {
	return bus.lifecycle.OperationError()
}

func encodeFrame(frame gocan.Frame, buffer *[fdMTU]byte) []byte {
	size := classicalMTU
	if frame.Flags.Has(gocan.FrameFD) {
		size = fdMTU
	}
	*buffer = [fdMTU]byte{}
	packet := buffer[:size]

	canID := frame.ID
	if frame.Flags.Has(gocan.FrameExtended) {
		canID |= unix.CAN_EFF_FLAG
	}
	if frame.Flags.Has(gocan.FrameRemote) {
		canID |= unix.CAN_RTR_FLAG
	}
	binary.NativeEndian.PutUint32(packet[:4], canID)

	if frame.Flags.Has(gocan.FrameRemote) {
		packet[4] = frame.DLC
	} else {
		packet[4] = byte(frame.DataLength())
		copy(packet[8:], frame.Data[:frame.DataLength()])
	}

	if frame.Flags.Has(gocan.FrameFD) {
		packet[5] = canFDFlexibleData
		if frame.Flags.Has(gocan.FrameBitRateSwitch) {
			packet[5] |= canFDBitRateSwitch
		}
		if frame.Flags.Has(gocan.FrameErrorStateIndicator) {
			packet[5] |= canFDErrorState
		}
	} else if frame.DLC > 8 {
		packet[7] = frame.DLC
	}
	return packet
}

func decodeFrame(packet []byte) (gocan.Frame, error) {
	if len(packet) != classicalMTU && len(packet) != fdMTU {
		return gocan.Frame{}, fmt.Errorf("unexpected SocketCAN frame size %d", len(packet))
	}

	canID := binary.NativeEndian.Uint32(packet[:4])
	if canID&unix.CAN_ERR_FLAG != 0 {
		return gocan.Frame{}, errors.New("SocketCAN error packet reached data-frame decoder")
	}

	var frame gocan.Frame
	if canID&unix.CAN_EFF_FLAG != 0 {
		frame.ID = canID & unix.CAN_EFF_MASK
		frame.Flags |= gocan.FrameExtended
	} else {
		frame.ID = canID & unix.CAN_SFF_MASK
	}
	if canID&unix.CAN_RTR_FLAG != 0 {
		frame.Flags |= gocan.FrameRemote
	}

	payloadLength := int(packet[4])
	if len(packet) == fdMTU {
		frame.Flags |= gocan.FrameFD
		if packet[5]&canFDBitRateSwitch != 0 {
			frame.Flags |= gocan.FrameBitRateSwitch
		}
		if packet[5]&canFDErrorState != 0 {
			frame.Flags |= gocan.FrameErrorStateIndicator
		}
		dlc, err := gocan.LengthToDLC(payloadLength, true)
		if err != nil {
			return gocan.Frame{}, err
		}
		frame.DLC = dlc
	} else {
		if payloadLength > 8 {
			return gocan.Frame{}, fmt.Errorf("classical SocketCAN payload length %d exceeds 8", payloadLength)
		}
		frame.DLC = uint8(payloadLength)
		if payloadLength == 8 && packet[7] > 8 && packet[7] <= 15 {
			frame.DLC = packet[7]
		}
	}

	if !frame.Flags.Has(gocan.FrameRemote) {
		copy(frame.Data[:], packet[8:8+payloadLength])
	}
	if err := frame.Validate(); err != nil {
		return gocan.Frame{}, err
	}
	return frame, nil
}

// decodedErrorPacket keeps the uncommon error path allocation-free. Combined
// Linux class bits can produce an error observation, a state observation, and
// a terminal receive-overrun observation from one packet.
type decodedErrorPacket struct {
	events     [3]gocan.Event
	eventCount uint8
	terminal   error
}

func decodeErrorPacket(packet []byte, bus gocan.BusID, timestamp time.Time) (decodedErrorPacket, error) {
	if len(packet) != classicalMTU {
		return decodedErrorPacket{}, fmt.Errorf("unexpected SocketCAN error frame size %d", len(packet))
	}

	canID := binary.NativeEndian.Uint32(packet[:4])
	if canID&unix.CAN_ERR_FLAG == 0 {
		return decodedErrorPacket{}, errors.New("SocketCAN packet is not an error frame")
	}
	if packet[4] != unix.CAN_ERR_DLC {
		return decodedErrorPacket{}, fmt.Errorf("unexpected SocketCAN error frame length %d", packet[4])
	}

	var result decodedErrorPacket
	classes := canID & unix.CAN_ERR_MASK
	controller := packet[9]
	if classes&canErrorObservationMask != 0 ||
		classes&unix.CAN_ERR_CRTL != 0 &&
			(controller == unix.CAN_ERR_CRTL_UNSPEC || controller&unix.CAN_ERR_CRTL_TX_OVERFLOW != 0) {
		event, err := gocan.NewErrorFrameEvent(bus, timestamp)
		if err != nil {
			return decodedErrorPacket{}, err
		}
		result.events[result.eventCount] = event
		result.eventCount++
	}

	state, hasState := socketCANControllerState(classes, controller)
	if hasState {
		countsKnown := classes&unix.CAN_ERR_CNT != 0
		var txErrorCount, rxErrorCount uint8
		if countsKnown {
			txErrorCount = packet[14]
			rxErrorCount = packet[15]
		}
		event, err := gocan.NewControllerStateEvent(
			bus,
			timestamp,
			state,
			txErrorCount,
			rxErrorCount,
			countsKnown,
		)
		if err != nil {
			return decodedErrorPacket{}, err
		}
		result.events[result.eventCount] = event
		result.eventCount++
	}

	if classes&unix.CAN_ERR_CRTL != 0 && controller&unix.CAN_ERR_CRTL_RX_OVERFLOW != 0 {
		event, err := gocan.NewReceiveOverrunEvent(bus, timestamp)
		if err != nil {
			return decodedErrorPacket{}, err
		}
		result.events[result.eventCount] = event
		result.eventCount++
		result.terminal = fmt.Errorf("%w: SocketCAN controller RX buffer overflow", gocan.ErrReceiveOverrun)
	} else if classes&unix.CAN_ERR_BUSOFF != 0 {
		result.terminal = fmt.Errorf("%w: SocketCAN controller entered bus-off", gocan.ErrBusOff)
	}
	return result, nil
}

func socketCANControllerState(classes uint32, controller uint8) (gocan.ControllerState, bool) {
	if classes&unix.CAN_ERR_BUSOFF != 0 {
		return gocan.ControllerBusOff, true
	}
	if classes&unix.CAN_ERR_RESTARTED != 0 {
		return gocan.ControllerActive, true
	}
	if classes&unix.CAN_ERR_CRTL != 0 && controller&unix.CAN_ERR_CRTL_ACTIVE != 0 {
		return gocan.ControllerActive, true
	}
	if classes&unix.CAN_ERR_CRTL == 0 {
		return 0, false
	}
	if controller&(unix.CAN_ERR_CRTL_RX_PASSIVE|unix.CAN_ERR_CRTL_TX_PASSIVE) != 0 {
		return gocan.ControllerPassive, true
	}
	if controller&(unix.CAN_ERR_CRTL_RX_WARNING|unix.CAN_ERR_CRTL_TX_WARNING) != 0 {
		return gocan.ControllerWarning, true
	}
	return 0, false
}
