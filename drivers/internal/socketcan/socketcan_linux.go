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

type Config struct {
	ID   gocan.BusID
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
		id:                 config.ID,
		name:               config.Name,
		ifIndex:            iface.Index,
		hasControllerState: true,
		capture:            capture,
		file:               file,
		raw:                raw,
		lifecycle: driverstate.New(func() {
			// Wake RawConn.Read without closing the descriptor from inside its
			// callback. receiveLoop closes it after the callback has returned.
			_ = file.SetReadDeadline(time.Now())
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

type Bus struct {
	id      gocan.BusID
	name    string
	ifIndex int
	capture *gocan.Capture
	file    *os.File
	raw     syscall.RawConn

	ioMu      sync.Mutex
	sendReady func(uintptr)
	txPacket  [fdMTU]byte
	txSize    int
	txSent    int
	txErr     error

	receiveReady       func(uintptr) bool
	rxPacket           [fdMTU]byte
	rxControl          [32]byte
	rxIovec            unix.Iovec
	rxMessage          unix.Msghdr
	rxSize             int
	rxControlSize      int
	rxFlags            int
	rxDropped          uint32
	busOff             bool
	busOffSeq          uint64
	busOffFallbackSeq  uint64
	hasControllerState bool
	stateProbeRunning  bool
	nextStateProbe     time.Time

	lifecycle *driverstate.Lifecycle
}

var _ gocan.Bus = (*Bus)(nil)

func (bus *Bus) ID() gocan.BusID {
	return bus.id
}

func (bus *Bus) Name() string {
	return bus.name
}

func (bus *Bus) Capture() *gocan.Capture {
	return bus.capture
}

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

	bus.ioMu.Lock()
	defer bus.ioMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bus.lifecycle.StopSignal():
		return bus.operationError()
	default:
	}
	if bus.busOff {
		bus.triggerBusOffProbe(false)
		return gocan.ErrBusOff
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
			if errors.Is(bus.txErr, unix.ENOBUFS) ||
				errors.Is(bus.txErr, unix.EAGAIN) ||
				errors.Is(bus.txErr, unix.EWOULDBLOCK) {
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
		Direction: gocan.DirectionTransmit,
		Frame:     frame,
	}
	if err := bus.capture.RecordFrame(event); err != nil {
		bus.stopWithError(err)
		return err
	}
	return nil
}

func (bus *Bus) Context() context.Context {
	return bus.lifecycle.Context()
}

func (bus *Bus) Close() error {
	bus.ioMu.Lock()
	bus.stopWithError(nil)
	bus.ioMu.Unlock()
	<-bus.lifecycle.Done()
	return nil
}

// receiveLoop waits without ioMu; receiveOnce locks only while reading and
// recording one packet. Closing waits until all native callbacks have returned.
func (bus *Bus) receiveLoop() {
	defer func() {
		bus.lifecycle.Stop(nil)
		bus.ioMu.Lock()
		_ = bus.file.Close()
		bus.ioMu.Unlock()
		bus.lifecycle.MarkDone()
	}()
	for {
		if err := bus.raw.Read(bus.receiveReady); err != nil {
			select {
			case <-bus.lifecycle.StopSignal():
			default:
				bus.stopWithError(fmt.Errorf("receive SocketCAN frame: %w", err))
			}
			return
		}
		select {
		case <-bus.lifecycle.StopSignal():
			return
		default:
		}
	}
}

// recordReceive is called with ioMu held immediately after a native read.
func (bus *Bus) recordReceive() error {
	lost, err := bus.receiveLost()
	if err != nil {
		return err
	}
	if lost {
		// On virtual interfaces without IFLA_CAN_STATE, clear cached bus-off
		// in case the socket drop discarded CAN_ERR_RESTARTED. Hardware
		// interfaces probe live kernel controller state asynchronously.
		if bus.busOff {
			if !bus.hasControllerState {
				bus.busOff = false
			} else {
				bus.busOffFallbackSeq = bus.busOffSeq
				bus.triggerBusOffProbe(false)
			}
		}
		if err := bus.capture.RecordEvent(gocan.Event{Bus: bus.id, Kind: gocan.EventReceiveOverrun}); err != nil {
			return err
		}
	}
	packet := bus.rxPacket[:bus.rxSize]
	if len(packet) >= 4 && binary.NativeEndian.Uint32(packet[:4])&unix.CAN_ERR_FLAG != 0 {
		errorPacket, err := decodeErrorPacket(packet, bus.id)
		if err != nil {
			return err
		}
		for index := range errorPacket.eventCount {
			event := errorPacket.events[index]
			if event.Kind == gocan.EventControllerState {
				if event.ControllerState == gocan.ControllerBusOff {
					bus.busOff = true
					bus.busOffSeq++
					bus.nextStateProbe = time.Time{}
					bus.triggerBusOffProbe(true)
				} else {
					bus.busOff = false
				}
			}
			if err := bus.capture.RecordEvent(event); err != nil {
				return err
			}
		}
		return nil
	}

	frame, err := decodeFrame(packet)
	if err != nil {
		return err
	}
	if bus.busOff {
		if !bus.hasControllerState {
			bus.busOff = false
		} else {
			bus.busOffFallbackSeq = bus.busOffSeq
			bus.triggerBusOffProbe(false)
		}
	}
	return bus.capture.RecordFrame(gocan.FrameEvent{
		Bus:       bus.id,
		Direction: gocan.DirectionReceive,
		Frame:     frame,
	})
}

const (
	nlaTypeMask         = 0x3fff
	busOffProbeInterval = 50 * time.Millisecond
	netlinkQueryTimeout = 50 * time.Millisecond
)

// triggerBusOffProbe starts at most one background RTM_GETLINK lookup per bus
// so Send never blocks on RTNL or USB status reads while still reconciling a
// dropped CAN_ERR_RESTARTED. Caller holds bus.ioMu.
func (bus *Bus) triggerBusOffProbe(immediate bool) {
	if !bus.busOff || !bus.hasControllerState || bus.stateProbeRunning {
		return
	}
	select {
	case <-bus.lifecycle.StopSignal():
		return
	default:
	}
	if !immediate && !bus.nextStateProbe.IsZero() && time.Now().Before(bus.nextStateProbe) {
		return
	}
	bus.stateProbeRunning = true
	seq := bus.busOffSeq
	ifIndex := bus.ifIndex
	go bus.probeBusOffState(seq, ifIndex)
}

func (bus *Bus) probeBusOffState(seq uint64, ifIndex int) {
	state, hasState, err := queryCANControllerState(ifIndex)
	bus.ioMu.Lock()
	if err == nil {
		if !hasState {
			bus.hasControllerState = false
			if bus.busOff && bus.busOffFallbackSeq == bus.busOffSeq {
				bus.busOff = false
			}
		} else if bus.busOffSeq == seq && state != unix.CAN_STATE_BUS_OFF {
			bus.busOff = false
		}
	}
	if bus.busOffSeq == seq {
		bus.nextStateProbe = time.Now().Add(busOffProbeInterval)
	}
	bus.stateProbeRunning = false
	bus.ioMu.Unlock()
}

// queryCANControllerState queries a single interface via RTM_GETLINK and
// returns its kernel IFLA_CAN_STATE when the interface is a "can" device.
// hasState is false with err == nil for interfaces confirmed to lack
// IFLA_CAN_STATE (such as vcan).
func queryCANControllerState(ifIndex int) (uint32, bool, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = unix.Close(fd) }()

	tv := unix.NsecToTimeval(netlinkQueryTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv); err != nil {
		return 0, false, err
	}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return 0, false, err
	}

	var req [unix.SizeofNlMsghdr + unix.SizeofIfInfomsg]byte
	binary.NativeEndian.PutUint32(req[0:4], uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:6], unix.RTM_GETLINK)
	binary.NativeEndian.PutUint16(req[6:8], unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(req[8:12], 1)
	req[unix.SizeofNlMsghdr] = unix.AF_UNSPEC
	binary.NativeEndian.PutUint32(req[unix.SizeofNlMsghdr+4:unix.SizeofNlMsghdr+8], uint32(ifIndex))

	deadline := time.Now().Add(netlinkQueryTimeout)
	dest := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	for {
		err = unix.Sendto(fd, req[:], 0, dest)
		if !errors.Is(err, unix.EINTR) {
			break
		}
		if !time.Now().Before(deadline) {
			return 0, false, context.DeadlineExceeded
		}
	}
	if err != nil {
		return 0, false, err
	}

	var resp [16384]byte
	var (
		n     int
		flags int
		from  unix.Sockaddr
	)
	for {
		n, _, flags, from, err = unix.Recvmsg(fd, resp[:], nil, 0)
		if !errors.Is(err, unix.EINTR) {
			break
		}
		if !time.Now().Before(deadline) {
			return 0, false, context.DeadlineExceeded
		}
	}
	if err != nil {
		return 0, false, err
	}
	if flags&unix.MSG_TRUNC != 0 {
		return 0, false, errors.New("SocketCAN netlink reply truncated")
	}
	if nlFrom, ok := from.(*unix.SockaddrNetlink); !ok || nlFrom.Pid != 0 {
		return 0, false, errors.New("unexpected SocketCAN netlink sender")
	}

	messages, err := syscall.ParseNetlinkMessage(resp[:n])
	if err != nil {
		return 0, false, err
	}
	for index := range messages {
		message := &messages[index]
		if message.Header.Type == unix.NLMSG_ERROR {
			return 0, false, errors.New("SocketCAN netlink error")
		}
		if message.Header.Type != syscall.RTM_NEWLINK || len(message.Data) < syscall.SizeofIfInfomsg {
			continue
		}
		info := (*syscall.IfInfomsg)(unsafe.Pointer(&message.Data[0]))
		if int(info.Index) != ifIndex {
			continue
		}
		attributes, err := syscall.ParseNetlinkRouteAttr(message)
		if err != nil {
			return 0, false, err
		}
		state, hasState := parseCANControllerState(attributes)
		return state, hasState, nil
	}
	return 0, false, errors.New("SocketCAN interface missing from netlink reply")
}

func parseCANControllerState(attributes []syscall.NetlinkRouteAttr) (uint32, bool) {
	for _, attribute := range attributes {
		if attribute.Attr.Type&nlaTypeMask != unix.IFLA_LINKINFO {
			continue
		}
		var kind string
		var infoData []byte
		for remaining := attribute.Value; len(remaining) >= 4; {
			attrType, payload, next, ok := nextNetlinkAttr(remaining)
			if !ok {
				return 0, false
			}
			switch attrType {
			case unix.IFLA_INFO_KIND:
				kind = unix.ByteSliceToString(payload)
			case unix.IFLA_INFO_DATA:
				infoData = payload
			}
			remaining = next
		}
		if kind != "can" || len(infoData) == 0 {
			return 0, false
		}
		for remaining := infoData; len(remaining) >= 4; {
			attrType, payload, next, ok := nextNetlinkAttr(remaining)
			if !ok {
				return 0, false
			}
			if attrType == unix.IFLA_CAN_STATE && len(payload) >= 4 {
				return binary.NativeEndian.Uint32(payload[:4]), true
			}
			remaining = next
		}
		return 0, false
	}
	return 0, false
}

func nextNetlinkAttr(raw []byte) (attrType uint16, payload, next []byte, ok bool) {
	if len(raw) < 4 {
		return 0, nil, nil, false
	}
	length := int(binary.NativeEndian.Uint16(raw[0:2]))
	if length < 4 || length > len(raw) {
		return 0, nil, nil, false
	}
	attrType = binary.NativeEndian.Uint16(raw[2:4]) & nlaTypeMask
	payload = raw[4:length]
	step := (length + 3) &^ 3
	if step >= len(raw) {
		return attrType, payload, nil, true
	}
	return attrType, payload, raw[step:], true
}

// receiveLost reports whether the socket dropped frames before this packet.
// SO_RXQ_OVFL carries the socket's cumulative drop count.
func (bus *Bus) receiveLost() (bool, error) {
	if bus.rxFlags&unix.MSG_CTRUNC != 0 {
		return false, errors.New("SocketCAN control message truncated")
	}
	if bus.rxControlSize == 0 {
		return false, nil
	}
	dropped, ok, err := socketReceiveDropCount(bus.rxControl[:bus.rxControlSize])
	if err != nil || !ok || dropped == bus.rxDropped {
		return false, err
	}
	bus.rxDropped = dropped
	return true, nil
}

func (bus *Bus) receiveOnce(fd uintptr) bool {
	bus.ioMu.Lock()
	defer bus.ioMu.Unlock()

	select {
	case <-bus.lifecycle.StopSignal():
		return true
	default:
	}
	bus.rxMessage.SetIovlen(1)
	bus.rxMessage.SetControllen(len(bus.rxControl))
	bus.rxMessage.Flags = 0

	read, _, errno := unix.Syscall(
		unix.SYS_RECVMSG,
		fd,
		uintptr(unsafe.Pointer(&bus.rxMessage)),
		unix.MSG_DONTWAIT,
	)
	if errno == unix.EAGAIN || errno == unix.EWOULDBLOCK {
		return false
	}
	if errno != 0 {
		bus.stopWithError(fmt.Errorf("receive SocketCAN frame: %w", errno))
		return true
	}
	bus.rxSize = int(read)
	bus.rxControlSize = int(bus.rxMessage.Controllen)
	bus.rxFlags = int(bus.rxMessage.Flags)
	if err := bus.recordReceive(); err != nil {
		bus.stopWithError(err)
	}
	return true
}

func socketReceiveDropCount(control []byte) (uint32, bool, error) {
	messages, err := unix.ParseSocketControlMessage(control)
	if err != nil {
		return 0, false, fmt.Errorf("parse SocketCAN receive metadata: %w", err)
	}
	for _, message := range messages {
		if message.Header.Level != unix.SOL_SOCKET || message.Header.Type != unix.SO_RXQ_OVFL {
			continue
		}
		if len(message.Data) < 4 {
			return 0, false, fmt.Errorf("SocketCAN receive-overrun metadata has length %d", len(message.Data))
		}
		return binary.NativeEndian.Uint32(message.Data[:4]), true, nil
	}
	return 0, false, nil
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
		packet[4] = min(frame.DLC, 8)
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
		// Linux UAPI len8_dlc: a classical DLC of 9-15, data or remote, travels
		// in byte 7 with len 8. Controllers honour it only with CAN_CTRLMODE_CC_LEN8_DLC set
		// (ip link ... cc-len8-dlc on); otherwise the frame goes out as DLC 8.
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
		// len8_dlc, as in encodeFrame: valid only alongside len 8.
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
// Linux class bits can produce an error, a state, and a receive-overrun
// observation from one packet.
type decodedErrorPacket struct {
	events     [3]gocan.Event
	eventCount uint8
}

func decodeErrorPacket(packet []byte, bus gocan.BusID) (decodedErrorPacket, error) {
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
		event := gocan.Event{Bus: bus, Kind: gocan.EventErrorFrame}
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
		event := gocan.Event{
			Bus:              bus,
			Kind:             gocan.EventControllerState,
			ControllerState:  state,
			TXErrorCount:     txErrorCount,
			RXErrorCount:     rxErrorCount,
			ErrorCountsKnown: countsKnown,
		}
		result.events[result.eventCount] = event
		result.eventCount++
	}

	if classes&unix.CAN_ERR_CRTL != 0 && controller&unix.CAN_ERR_CRTL_RX_OVERFLOW != 0 {
		event := gocan.Event{Bus: bus, Kind: gocan.EventReceiveOverrun}
		result.events[result.eventCount] = event
		result.eventCount++
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
