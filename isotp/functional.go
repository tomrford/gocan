package isotp

import (
	"context"
	"errors"
	"time"

	"github.com/tomrford/gocan"
)

// FunctionalConfig defines one functional, normal-addressing ISO-TP send path.
// Each field has the meaning documented on the Config field of the same name.
// Functional paths send only Single Frames, so they have no receive or
// flow-control settings, and acceptance does not confirm delivery.
type FunctionalConfig struct {
	TransmitID            uint32
	FrameFlags            gocan.FrameFlags
	TransmitDataLength    uint8
	TransmitMinDataLength uint8
	PaddingByte           byte
	TransmitRetryTimeout  time.Duration
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
		return nil, errors.New("isotp: functional path requires a bus")
	}
	transmitter, err := newTransmitter(config)
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
	return gocan.Send(ctx, functional.bus, transmission.firstFrame, functional.transmitRetryTimeout)
}
