package xcp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
)

// Wire layouts are corroborated by Vector XCPlite xcp.h (CONNECT/GET_STATUS/
// SET_MTA/UPLOAD) at 52b52a95bbabdf4cf46bf141418eb23d00c5ecad and pyXCP
// types.py/master.py at f1b547b23f1ad56cb3b201c1790d941931f45f0a.
// CAN padding configuration is described by Vector's XCP_104.aml CAN_Parameters.
func (client *Client) command(ctx context.Context, request Request) (Response, error) {
	retryConnect := request.Command == CommandConnect && client.pending == CommandConnect
	minimum, known, err := client.validateRequest(request)
	if err != nil {
		return Response{}, err
	}
	if err := context.Cause(ctx); err != nil {
		return Response{}, err
	}
	packet := append([]byte{byte(request.Command)}, request.Data...)
	length := len(packet)
	if client.config.PadFrames {
		length = int(client.config.TransmitDataLength)
	}
	for {
		if _, err := gocan.LengthToDLC(length, client.config.FrameFlags.Has(gocan.FrameFD)); err == nil {
			break
		}
		length++
	}
	for len(packet) < length {
		packet = append(packet, client.config.PaddingByte)
	}
	frame, err := gocan.NewFrame(client.config.TransmitID, packet, client.config.FrameFlags)
	if err != nil {
		return Response{}, err
	}
	client.mu.Lock()
	client.cursor = client.capture.End()
	client.retaining = true
	client.mu.Unlock()
	if err := client.bus.Send(ctx, frame); err != nil {
		// Bus.Send reports a definite native result. A rejected send has created
		// no outstanding command, so it must not poison a synchronised session.
		if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return Response{}, context.Cause(ctx)
		}
		return Response{}, err
	}
	if !known || request.Command == CommandConnect || request.Command == CommandDisconnect {
		client.connected = false
	}
	client.pending = request.Command
	response, err := client.receive(ctx, request.Command, minimum)
	if err != nil {
		if retryConnect && !errors.Is(err, ErrSessionTerminated) {
			client.pending = CommandConnect
		}
		return Response{}, err
	}
	if request.Command == CommandConnect {
		if retryConnect {
			// Normal CONNECT can be retried while the ECU is still disconnected
			// (pyXCP errormatrix.py). Fence late duplicate CONNECT replies before
			// trusting this result. A failed barrier must not permit another
			// CONNECT: an older SYNCH marker could then precede that new request.
			if _, err := client.command(ctx, Request{Command: CommandSynch}); err != nil {
				return Response{}, err
			}
			// The candidate reply may describe an earlier connection. Read fresh
			// capabilities only after all pre-barrier CONNECT replies are fenced.
			return client.command(ctx, request)
		}
		capabilities, err := client.parseConnect(response.Data)
		if err != nil {
			return Response{}, err
		}
		client.capabilities = capabilities
		client.connected = true
	}
	client.pending = 0
	return response, nil
}

func (client *Client) validateRequest(request Request) (minimum int, known bool, err error) {
	if client.pending != 0 && request.Command != CommandSynch && !(request.Command == CommandConnect && client.pending == CommandConnect) {
		return 0, false, ErrSynchronizationRequired
	}
	if !client.connected && request.Command != CommandConnect && request.Command != CommandSynch && request.Command != CommandDisconnect {
		return 0, false, ErrNotConnected
	}
	maximum := int(client.config.TransmitDataLength)
	if client.connected && request.Command != CommandConnect {
		maximum = int(client.capabilities.MaxCTO)
	}
	if request.Command < 0xc0 || len(request.Data)+1 > maximum {
		return 0, false, fmt.Errorf("%w: command or packet size", ErrUnsupported)
	}
	parameters := -1
	minimum = 1
	known = true
	switch request.Command {
	case CommandConnect:
		parameters, minimum = 1, 8
		if len(request.Data) == 1 && request.Data[0] != 0 {
			return 0, true, fmt.Errorf("%w: only normal CONNECT mode", ErrUnsupported)
		}
	case CommandDisconnect, CommandSynch:
		parameters = 0
	case CommandGetStatus:
		parameters, minimum = 0, 6
	case CommandGetCommModeInfo:
		parameters, minimum = 0, 8
		if client.capabilities.CommunicationMode&0x80 == 0 {
			return 0, true, fmt.Errorf("%w: optional communication information not advertised", ErrUnsupported)
		}
	case CommandSetMTA:
		parameters = 7
	case CommandUpload, CommandShortUpload:
		parameters = 1
		if request.Command == CommandShortUpload {
			parameters = 7
		}
		if client.capabilities.AddressGranularity != 1 {
			return 0, true, fmt.Errorf("%w: memory reads require byte address granularity", ErrUnsupported)
		}
		if len(request.Data) > 0 {
			minimum = 1 + int(request.Data[0])
			if minimum < 2 || minimum > maximum {
				return 0, true, fmt.Errorf("%w: read count must fit one response", ErrUnsupported)
			}
		}
	default:
		known = false
	}
	if parameters >= 0 && len(request.Data) != parameters {
		return 0, known, fmt.Errorf("XCP command %#02x needs %d parameter bytes", request.Command, parameters)
	}
	if request.Command == CommandShortUpload {
		address := client.capabilities.ByteOrder.Uint32(request.Data[3:])
		if uint64(address)+uint64(request.Data[0]) > uint64(^uint32(0)) {
			return 0, known, fmt.Errorf("XCP read address/count overflow")
		}
	}
	return minimum, known, nil
}

func (client *Client) receive(ctx context.Context, command Command, minimum int) (Response, error) {
	deadline := time.Now().Add(client.config.Timeout)
	for {
		wait, cancel := context.WithDeadlineCause(ctx, deadline, ErrTimeout)
		client.mu.Lock()
		cursor := client.cursor
		client.mu.Unlock()
		event, next, err := client.capture.Next(wait, client.key, cursor)
		cause := context.Cause(wait)
		cancel()
		// Next preserves a parked waiter across Clear/Prune. Validate its old
		// position before advancing, otherwise a reply from a new capture
		// generation could silently bridge lost command history.
		if _, err := client.capture.FramesBetween(cursor, cursor); err != nil {
			return Response{}, err
		}
		if err != nil {
			if cause != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return Response{}, cause
			}
			return Response{}, err
		}
		// Capture can return buffered traffic immediately, so context/deadline
		// checks must not depend on the reader ever blocking.
		if err := context.Cause(ctx); err != nil {
			return Response{}, err
		}
		if !time.Now().Before(deadline) {
			return Response{}, ErrTimeout
		}
		client.mu.Lock()
		client.cursor = next
		client.mu.Unlock()
		frame := event.Frame
		if frame.Flags.Has(gocan.FrameRemote) || frame.Flags.Has(gocan.FrameFD) != client.config.FrameFlags.Has(gocan.FrameFD) {
			continue
		}
		length := frame.DataLength()
		if length == 0 || length > int(client.config.TransmitDataLength) {
			return Response{}, fmt.Errorf("%w: CAN packet length %d", ErrInvalidResponse, length)
		}
		data := frame.Data[:length]
		switch data[0] {
		case 0xff, 0xfe:
			if data[0] == 0xfe && length < 2 {
				return Response{}, fmt.Errorf("%w: truncated ERR", ErrInvalidResponse)
			}
			if command == CommandSynch {
				if data[0] == 0xfe && data[1] == 0 {
					return Response{Data: append([]byte(nil), data[1:]...)}, nil
				}
				continue // A late ordinary response is not the synchronisation marker.
			}
			if data[0] == 0xfe {
				if data[1] != 0 {
					client.pending = 0
				}
				return Response{}, &NegativeResponseError{Command: command, Code: data[1]}
			}
			if length < minimum {
				return Response{}, fmt.Errorf("%w: command %#02x response length %d, need %d", ErrInvalidResponse, command, length, minimum)
			}
			return Response{Data: append([]byte(nil), data[1:]...)}, nil
		case 0xfd:
			if length < 2 {
				return Response{}, fmt.Errorf("%w: truncated EV", ErrInvalidResponse)
			}
			switch data[1] {
			case 0x05:
				deadline = time.Now().Add(client.config.Timeout) // EV_CMD_PENDING
			case 0x07:
				client.connected = false
				client.pending = 0 // Explicit ECU disconnect; SYNCH requires a live session.
				return Response{}, ErrSessionTerminated
			}
			// SERV (0xfc), DAQ (0x00..0xfb), and other events remain in Capture.
		}
	}
}

func (client *Client) parseConnect(data []byte) (Capabilities, error) {
	var order binary.ByteOrder = binary.LittleEndian
	if data[1]&1 != 0 {
		order = binary.BigEndian
	}
	granularity := (data[1] >> 1) & 3
	maxCTO, maxDTO := data[2], order.Uint16(data[3:5])
	capacity := client.config.TransmitDataLength
	if granularity == 3 || maxCTO < 8 || maxCTO > capacity || maxDTO < 8 || maxDTO > uint16(capacity) {
		return Capabilities{}, fmt.Errorf("%w: CONNECT granularity or packet limits", ErrInvalidResponse)
	}
	if data[5] != 1 || data[6] != 1 {
		return Capabilities{}, fmt.Errorf("%w: XCP protocol/transport major versions %d/%d", ErrUnsupported, data[5], data[6])
	}
	return Capabilities{Resources: data[0], CommunicationMode: data[1], ByteOrder: order, AddressGranularity: 1 << granularity,
		MaxCTO: maxCTO, MaxDTO: maxDTO, ProtocolVersion: data[5], TransportVersion: data[6]}, nil
}
