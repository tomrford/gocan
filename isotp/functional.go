package isotp

import (
	"context"
	"errors"

	"github.com/tomrford/gocan"
)

// FunctionalConfig defines one functional, normal-addressing ISO-TP send path.
type FunctionalConfig struct {
	TransmitID uint32
	FrameFlags gocan.FrameFlags

	// TransmitDataLength is the maximum CAN data length used for transmitted
	// frames. Zero selects 8. CAN FD additionally permits 12, 16, 20, 24, 32,
	// 48, or 64.
	TransmitDataLength uint8
	// PadFrames pads every transmitted frame to TransmitDataLength. CAN FD
	// frames are always padded as needed to reach a legal CAN FD data length.
	PadFrames bool
	// PaddingByte fills both requested padding and the padding required to reach
	// a legal CAN FD data length.
	PaddingByte byte
}

// Functional transmits functionally addressed ISO-TP payloads. ISO 15765-2
// restricts functional addressing to Single Frames, so every payload must fit
// one CAN frame, and there is no receive path: servers that answer do so on
// their own physical addresses.
type Functional struct {
	bus gocan.Bus
	transmitter
}

// NewFunctional validates config and prepares a functional, normal-addressing
// ISO-TP send path.
func NewFunctional(bus gocan.Bus, config FunctionalConfig) (*Functional, error) {
	if bus == nil {
		return nil, errors.New("ISO-TP functional path requires a bus")
	}
	transmitter, err := newTransmitter(config.TransmitID, config.FrameFlags, config.TransmitDataLength, config.PadFrames, config.PaddingByte)
	if err != nil {
		return nil, err
	}
	transmitter.maximumPayloadLength = transmitter.singleFrameCapacity()
	return &Functional{bus: bus, transmitter: transmitter}, nil
}

// Send transmits one payload in one Single Frame. A payload that does not fit
// the configured transmit data length reports ErrPayloadTooLarge.
func (functional *Functional) Send(ctx context.Context, payload []byte) error {
	transmission, err := functional.prepareTransmission(payload)
	if err != nil {
		return err
	}
	return functional.bus.Send(ctx, transmission.firstFrame)
}
