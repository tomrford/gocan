package isotp

import (
	"errors"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
)

type frameType uint8

const (
	frameSingle frameType = iota
	frameFirst
	frameConsecutive
	frameFlowControl
)

type flowStatus uint8

const (
	flowContinue flowStatus = iota
	flowWait
	flowOverflow
)

type pdu struct {
	kind    frameType
	payload []byte
	length  uint32
	// dataLength is the CAN data length the frame arrived with, used to reject a
	// Consecutive Frame that is short without being the last one.
	dataLength     int
	sequence       uint8
	flowStatus     flowStatus
	blockSize      uint8
	separationTime time.Duration
}

type transmission struct {
	firstFrame gocan.Frame
	payload    []byte
	offset     int
	multiFrame bool
}

func (link *Link) prepareTransmission(payload []byte) (transmission, error) {
	if len(payload) == 0 {
		return transmission{}, errors.New("ISO-TP payload must not be empty")
	}
	if uint64(len(payload)) > uint64(link.maximumPayloadLength) {
		return transmission{}, fmt.Errorf("%w: %d exceeds configured maximum %d", ErrPayloadTooLarge, len(payload), link.maximumPayloadLength)
	}

	escapeSingleFrame := len(payload) > 7 || link.padFrames && link.transmitDataLength > 8
	singleHeaderLength := 1
	if escapeSingleFrame {
		singleHeaderLength = 2
	}
	if len(payload) <= link.transmitDataLength-singleHeaderLength {
		data := make([]byte, singleHeaderLength+len(payload))
		if escapeSingleFrame {
			data[1] = byte(len(payload))
		} else {
			data[0] = byte(len(payload))
		}
		copy(data[singleHeaderLength:], payload)
		frame, err := link.makeFrame(data, false)
		return transmission{firstFrame: frame}, err
	}

	headerLength := 2
	if len(payload) > 0x0fff {
		headerLength = 6
	}
	data := make([]byte, link.transmitDataLength)
	if headerLength == 2 {
		data[0] = 0x10 | byte(len(payload)>>8)
		data[1] = byte(len(payload))
	} else {
		length := uint32(len(payload))
		data[0] = 0x10
		data[1] = 0
		data[2] = byte(length >> 24)
		data[3] = byte(length >> 16)
		data[4] = byte(length >> 8)
		data[5] = byte(length)
	}
	offset := copy(data[headerLength:], payload)
	frame, err := link.makeFrame(data, true)
	if err != nil {
		return transmission{}, err
	}
	return transmission{
		firstFrame: frame,
		payload:    payload,
		offset:     offset,
		multiFrame: true,
	}, nil
}

func (link *Link) makeFrame(data []byte, fullLength bool) (gocan.Frame, error) {
	if len(data) > link.transmitDataLength {
		return gocan.Frame{}, fmt.Errorf("%w: %d-byte transport frame exceeds transmit data length %d", ErrProtocol, len(data), link.transmitDataLength)
	}
	targetLength := len(data)
	if fullLength || link.padFrames {
		targetLength = link.transmitDataLength
	} else if link.frameFlags.Has(gocan.FrameFD) {
		targetLength = nearestCANFDLength(targetLength)
	}
	if targetLength < len(data) || targetLength > link.transmitDataLength {
		return gocan.Frame{}, fmt.Errorf("%w: invalid transport frame length %d", ErrProtocol, targetLength)
	}
	if targetLength != len(data) {
		padded := make([]byte, targetLength)
		copy(padded, data)
		for index := len(data); index < len(padded); index++ {
			padded[index] = link.paddingByte
		}
		data = padded
	}
	return gocan.NewFrame(link.transmitID, data, link.frameFlags)
}

func parseFrame(frame gocan.Frame) (pdu, error) {
	dataLength := frame.DataLength()
	if dataLength == 0 {
		return pdu{}, fmt.Errorf("%w: empty CAN frame", ErrProtocol)
	}
	data := frame.Data[:dataLength]
	parsed := pdu{
		kind:       frameType(data[0] >> 4),
		dataLength: dataLength,
	}

	switch parsed.kind {
	case frameSingle:
		length := int(data[0] & 0x0f)
		start := 1
		if length == 0 {
			if !frame.Flags.Has(gocan.FrameFD) || dataLength <= 8 || dataLength < 2 {
				return pdu{}, fmt.Errorf("%w: invalid Single Frame escape length", ErrProtocol)
			}
			length = int(data[1])
			start = 2
			if length == 0 {
				return pdu{}, fmt.Errorf("%w: Single Frame payload is empty", ErrProtocol)
			}
		} else if dataLength > 8 {
			return pdu{}, fmt.Errorf("%w: CAN FD Single Frame is missing its escape length", ErrProtocol)
		}
		if length > dataLength-start {
			return pdu{}, fmt.Errorf("%w: Single Frame length %d exceeds its data", ErrProtocol, length)
		}
		parsed.payload = data[start : start+length]

	case frameFirst:
		// A First Frame must fill its CAN frame, which also guarantees room for
		// either length form. Classical CAN therefore requires exactly 8 bytes.
		if dataLength < 8 {
			return pdu{}, fmt.Errorf("%w: First Frame carries %d bytes and does not fill its CAN frame", ErrProtocol, dataLength)
		}
		length := uint32(data[0]&0x0f)<<8 | uint32(data[1])
		start := 2
		if length == 0 {
			length = uint32(data[2])<<24 | uint32(data[3])<<16 | uint32(data[4])<<8 | uint32(data[5])
			start = 6
			if length <= 0x0fff {
				return pdu{}, fmt.Errorf("%w: extended First Frame length %d does not require extended encoding", ErrProtocol, length)
			}
		}
		parsed.length = length
		parsed.payload = data[start:]

	case frameConsecutive:
		if dataLength < 2 {
			return pdu{}, fmt.Errorf("%w: Consecutive Frame has no payload", ErrProtocol)
		}
		parsed.sequence = data[0] & 0x0f
		parsed.payload = data[1:]

	case frameFlowControl:
		if dataLength < 3 {
			return pdu{}, fmt.Errorf("%w: Flow Control frame is shorter than three bytes", ErrProtocol)
		}
		parsed.flowStatus = flowStatus(data[0] & 0x0f)
		if parsed.flowStatus > flowOverflow {
			return pdu{}, fmt.Errorf("%w: reserved Flow Control status %#x", ErrProtocol, parsed.flowStatus)
		}
		parsed.blockSize = data[1]
		parsed.separationTime = decodeSeparationTime(data[2])

	default:
		return pdu{}, fmt.Errorf("%w: reserved frame type %#x", ErrProtocol, parsed.kind)
	}
	return parsed, nil
}

func nearestCANFDLength(length int) int {
	switch {
	case length <= 8:
		return length
	case length <= 12:
		return 12
	case length <= 16:
		return 16
	case length <= 20:
		return 20
	case length <= 24:
		return 24
	case length <= 32:
		return 32
	case length <= 48:
		return 48
	default:
		return 64
	}
}

// decodeSeparationTime converts a Flow Control STmin byte. A reserved encoding
// becomes the longest defined separation time rather than an error, because
// ISO 15765-2 requires a sender that receives one to slow to 127 ms instead of
// abandoning the transfer.
func decodeSeparationTime(value byte) time.Duration {
	switch {
	case value <= 0x7f:
		return time.Duration(value) * time.Millisecond
	case value >= 0xf1 && value <= 0xf9:
		return time.Duration(value-0xf0) * 100 * time.Microsecond
	default:
		return 127 * time.Millisecond
	}
}
