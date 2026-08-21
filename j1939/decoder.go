package j1939

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tomrford/gocan"
)

const (
	transportControlPGN PGN = 0xec00
	transportDataPGN    PGN = 0xeb00

	transportRTS   byte = 0x10
	transportCTS   byte = 0x11
	transportEOMA  byte = 0x13
	transportBAM   byte = 0x20
	transportAbort byte = 0xff

	maximumTransportPayload = 1785
)

// ErrProtocol identifies malformed or inconsistent J1939 transport traffic.
var ErrProtocol = errors.New("J1939 protocol error")

// Message is one complete J1939 parameter group reconstructed from one or more
// raw CAN frames. Payload owns its storage.
//
// Priority is the CAN priority of a single frame or of the connection-management
// frame that announced a transport transfer. Destination is the transport peer
// for a connection-managed transfer, including when PGN is a PDU2 PGN.
type Message struct {
	Bus         gocan.BusID
	Direction   gocan.Direction
	Priority    uint8
	PGN         PGN
	Source      Address
	Destination Address
	Payload     []byte
	StartedAt   time.Time
	CompletedAt time.Time
}

// Diagnostic reports one frame that could not be applied to the passive
// decoder. Processing later frames may continue.
type Diagnostic struct {
	Bus       gocan.BusID
	Direction gocan.Direction
	Timestamp time.Time
	Err       error
}

func (diagnostic Diagnostic) Error() string {
	return diagnostic.Err.Error()
}

func (diagnostic Diagnostic) Unwrap() error {
	return diagnostic.Err
}

// Decoder passively reconstructs J1939 messages from FrameEvents in capture
// order. The zero Decoder is ready for use. It sends no flow-control frames and
// does not participate in address claiming.
type Decoder struct {
	sessions map[sessionKey]*session
}

type sessionKey struct {
	bus         gocan.BusID
	direction   gocan.Direction
	source      Address
	destination Address
}

type session struct {
	message Message
	size    int
	packets uint8
	next    uint8
	lastAt  time.Time
}

// Push consumes one raw frame. It returns a complete single-frame or transport
// message when this frame finishes one.
func (decoder *Decoder) Push(event gocan.FrameEvent) (Message, bool, error) {
	if err := event.Validate(); err != nil {
		return Message{}, false, err
	}
	frame := event.Frame
	if !frame.Flags.Has(gocan.FrameExtended) ||
		frame.Flags.Has(gocan.FrameRemote) ||
		frame.Flags.Has(gocan.FrameFD) {
		return Message{}, false, nil
	}

	header, err := ParseID(frame.ID)
	if err != nil {
		return Message{}, false, err
	}
	switch header.PGN {
	case transportControlPGN:
		return decoder.pushControl(event, header)
	case transportDataPGN:
		return decoder.pushData(event, header)
	default:
		length := frame.DataLength()
		payload := append([]byte(nil), frame.Data[:length]...)
		return Message{
			Bus:         event.Bus,
			Direction:   event.Direction,
			Priority:    header.Priority,
			PGN:         header.PGN,
			Source:      header.Source,
			Destination: header.Destination,
			Payload:     payload,
			StartedAt:   event.Timestamp,
			CompletedAt: event.Timestamp,
		}, true, nil
	}
}

// PushBatch consumes frames in order and continues after malformed transport
// traffic. Diagnostics retain the frame metadata for every rejected operation.
func (decoder *Decoder) PushBatch(events []gocan.FrameEvent) ([]Message, []Diagnostic) {
	var messages []Message
	var diagnostics []Diagnostic
	for _, event := range events {
		message, complete, err := decoder.Push(event)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Bus:       event.Bus,
				Direction: event.Direction,
				Timestamp: event.Timestamp,
				Err:       err,
			})
			continue
		}
		if complete {
			messages = append(messages, message)
		}
	}
	return messages, diagnostics
}

// Reset discards every incomplete transport session. Use it after capture
// history is lost and the caller resynchronises its cursor.
func (decoder *Decoder) Reset() {
	clear(decoder.sessions)
}

// Flush reports and discards every transport session left incomplete at the
// end of a finite input. Diagnostics are ordered by the time of the last frame
// applied to each session.
func (decoder *Decoder) Flush() []Diagnostic {
	diagnostics := make([]Diagnostic, 0, len(decoder.sessions))
	for key, active := range decoder.sessions {
		diagnostics = append(diagnostics, Diagnostic{
			Bus:       key.bus,
			Direction: key.direction,
			Timestamp: active.lastAt,
			Err: fmt.Errorf(
				"%w: TP transfer from %#02x to %#02x for PGN %#x ended after %d of %d bytes",
				ErrProtocol,
				key.source,
				key.destination,
				active.message.PGN,
				len(active.message.Payload),
				active.size,
			),
		})
	}
	slices.SortFunc(diagnostics, func(left, right Diagnostic) int {
		return left.Timestamp.Compare(right.Timestamp)
	})
	decoder.Reset()
	return diagnostics
}

func (decoder *Decoder) pushControl(event gocan.FrameEvent, header Header) (Message, bool, error) {
	data, err := transportData(event.Frame, "TP.CM")
	if err != nil {
		return Message{}, false, err
	}

	switch data[0] {
	case transportBAM, transportRTS:
		return Message{}, false, decoder.startSession(event, header, data)
	case transportAbort:
		return Message{}, false, decoder.abortSession(event, header, data)
	case transportCTS, transportEOMA:
		return Message{}, false, nil
	default:
		return Message{}, false, fmt.Errorf("%w: unknown TP.CM control %#02x", ErrProtocol, data[0])
	}
}

func (decoder *Decoder) startSession(event gocan.FrameEvent, header Header, data []byte) error {
	broadcast := data[0] == transportBAM
	if broadcast && header.Destination != GlobalAddress {
		return fmt.Errorf("%w: TP.BAM destination is %#02x, want global", ErrProtocol, header.Destination)
	}
	if !broadcast && header.Destination == GlobalAddress {
		return fmt.Errorf("%w: TP.RTS destination must be specific", ErrProtocol)
	}
	if broadcast && data[4] != 0xff {
		return fmt.Errorf("%w: TP.BAM reserved byte is %#02x, want 0xff", ErrProtocol, data[4])
	}

	size := int(binary.LittleEndian.Uint16(data[1:3]))
	packets := data[3]
	if size <= 8 || size > maximumTransportPayload {
		return fmt.Errorf("%w: TP payload length %d is outside 9 through %d", ErrProtocol, size, maximumTransportPayload)
	}
	wantPackets := (size + 6) / 7
	if int(packets) != wantPackets {
		return fmt.Errorf("%w: TP packet count %d does not match %d-byte payload", ErrProtocol, packets, size)
	}
	pgn, err := controlPGN(data)
	if err != nil {
		return err
	}

	key := sessionKey{
		bus:         event.Bus,
		direction:   event.Direction,
		source:      header.Source,
		destination: header.Destination,
	}
	previous := decoder.sessions[key]
	if decoder.sessions == nil {
		decoder.sessions = make(map[sessionKey]*session)
	}
	decoder.sessions[key] = &session{
		message: Message{
			Bus:         event.Bus,
			Direction:   event.Direction,
			Priority:    header.Priority,
			PGN:         pgn,
			Source:      header.Source,
			Destination: header.Destination,
			Payload:     make([]byte, 0, size),
			StartedAt:   event.Timestamp,
		},
		size:    size,
		packets: packets,
		next:    1,
		lastAt:  event.Timestamp,
	}
	if previous != nil {
		return fmt.Errorf(
			"%w: new TP transfer from %#02x to %#02x replaced incomplete PGN %#x",
			ErrProtocol,
			key.source,
			key.destination,
			previous.message.PGN,
		)
	}
	return nil
}

func (decoder *Decoder) pushData(event gocan.FrameEvent, header Header) (Message, bool, error) {
	data, err := transportData(event.Frame, "TP.DT")
	if err != nil {
		return Message{}, false, err
	}
	key := sessionKey{
		bus:         event.Bus,
		direction:   event.Direction,
		source:      header.Source,
		destination: header.Destination,
	}
	active := decoder.sessions[key]
	if active == nil {
		return Message{}, false, fmt.Errorf(
			"%w: TP.DT packet %d from %#02x to %#02x has no active transfer",
			ErrProtocol,
			data[0],
			header.Source,
			header.Destination,
		)
	}
	sequence := data[0]
	if sequence != active.next {
		delete(decoder.sessions, key)
		return Message{}, false, fmt.Errorf(
			"%w: TP.DT sequence %d from %#02x to %#02x, want %d",
			ErrProtocol,
			sequence,
			header.Source,
			header.Destination,
			active.next,
		)
	}

	remaining := active.size - len(active.message.Payload)
	payload := data[1:]
	if len(payload) > remaining {
		payload = payload[:remaining]
	}
	active.message.Payload = append(active.message.Payload, payload...)
	active.next++
	active.lastAt = event.Timestamp
	if len(active.message.Payload) < active.size {
		return Message{}, false, nil
	}
	if sequence != active.packets {
		delete(decoder.sessions, key)
		return Message{}, false, fmt.Errorf(
			"%w: TP transfer completed on packet %d, announced %d",
			ErrProtocol,
			sequence,
			active.packets,
		)
	}

	delete(decoder.sessions, key)
	active.message.CompletedAt = event.Timestamp
	return active.message, true, nil
}

func (decoder *Decoder) abortSession(event gocan.FrameEvent, header Header, data []byte) error {
	pgn, err := controlPGN(data)
	if err != nil {
		return err
	}
	candidates := [...]sessionKey{
		{bus: event.Bus, direction: event.Direction, source: header.Source, destination: header.Destination},
		{bus: event.Bus, direction: event.Direction, source: header.Destination, destination: header.Source},
		{bus: event.Bus, direction: oppositeDirection(event.Direction), source: header.Destination, destination: header.Source},
	}
	for _, key := range candidates {
		if active := decoder.sessions[key]; active != nil && active.message.PGN == pgn {
			delete(decoder.sessions, key)
			return fmt.Errorf(
				"%w: TP transfer from %#02x to %#02x for PGN %#x was aborted with reason %#02x",
				ErrProtocol,
				key.source,
				key.destination,
				pgn,
				data[1],
			)
		}
	}
	return nil
}

func transportData(frame gocan.Frame, name string) ([]byte, error) {
	if frame.DataLength() != 8 {
		return nil, fmt.Errorf("%w: %s frame has %d data bytes, want 8", ErrProtocol, name, frame.DataLength())
	}
	return frame.Data[:8], nil
}

func controlPGN(data []byte) (PGN, error) {
	pgn := PGN(data[5]) | PGN(data[6])<<8 | PGN(data[7])<<16
	if err := pgn.validate(); err != nil {
		return 0, fmt.Errorf("%w: TP connection-management target: %v", ErrProtocol, err)
	}
	return pgn, nil
}

func oppositeDirection(direction gocan.Direction) gocan.Direction {
	if direction == gocan.DirectionReceive {
		return gocan.DirectionTransmit
	}
	return gocan.DirectionReceive
}
