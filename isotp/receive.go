package isotp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// receive reassembles one complete payload. Callers must hold the receiving
// token, so one reassembly is never interleaved with another.
func (link *Link) receive(ctx context.Context, firstFrameTimeout time.Duration) ([]byte, error) {
	firstFrameContext := ctx
	cancel := func() {}
	if firstFrameTimeout > 0 {
		firstFrameContext, cancel = context.WithTimeout(ctx, firstFrameTimeout)
	}
	frame, err := link.nextFrame(firstFrameContext)
	cancel()
	if err != nil {
		return nil, err
	}
	first, err := parseFrame(frame.Frame)
	if err != nil {
		return nil, err
	}

	switch first.kind {
	case frameSingle:
		if uint32(len(first.payload)) > link.maximumPayloadLength {
			return nil, fmt.Errorf("%w: %d exceeds configured maximum %d", ErrPayloadTooLarge, len(first.payload), link.maximumPayloadLength)
		}
		payload := make([]byte, len(first.payload))
		copy(payload, first.payload)
		return payload, nil
	case frameFirst:
		return link.receiveMultiFrame(ctx, first)
	default:
		return nil, fmt.Errorf("%w: expected a Single or First Frame, got type %#x", ErrProtocol, first.kind)
	}
}

func (link *Link) receiveMultiFrame(ctx context.Context, first pdu) ([]byte, error) {
	if first.length <= 7 {
		return nil, fmt.Errorf("%w: First Frame length %d must use a Single Frame", ErrProtocol, first.length)
	}
	if first.length > link.maximumPayloadLength || uint64(first.length) > uint64(math.MaxInt) {
		payloadError := fmt.Errorf("%w: %d exceeds configured maximum %d", ErrPayloadTooLarge, first.length, link.maximumPayloadLength)
		if err := link.sendFlowControl(ctx, flowOverflow); err != nil {
			return nil, errors.Join(payloadError, err)
		}
		return nil, payloadError
	}
	total := int(first.length)
	if total <= len(first.payload) {
		return nil, fmt.Errorf("%w: First Frame length %d does not require segmentation", ErrProtocol, total)
	}

	payload := make([]byte, 0, total)
	payload = append(payload, first.payload...)
	if err := link.sendFlowControl(ctx, flowContinue); err != nil {
		return nil, err
	}

	expectedSequence := uint8(1)
	blockCount := uint8(0)
	for len(payload) < total {
		consecutive, err := link.nextPDUWithTimeout(ctx, link.consecutiveFrameTimeout, ErrConsecutiveFrameTimeout)
		if err != nil {
			return nil, err
		}
		if consecutive.kind != frameConsecutive {
			return nil, fmt.Errorf("%w: expected a Consecutive Frame, got type %#x", ErrProtocol, consecutive.kind)
		}
		if consecutive.sequence != expectedSequence {
			return nil, fmt.Errorf("%w: Consecutive Frame sequence %#x, expected %#x", ErrProtocol, consecutive.sequence, expectedSequence)
		}

		// Every Consecutive Frame but the last must use the First Frame's CAN
		// data length. A short frame that does not complete the payload means the
		// peer either lost data or is not segmenting to the standard.
		remaining := total - len(payload)
		if len(consecutive.payload) < remaining && consecutive.dataLength != first.dataLength {
			return nil, fmt.Errorf("%w: Consecutive Frame carries %d bytes before the final frame, expected the First Frame's %d", ErrProtocol, consecutive.dataLength, first.dataLength)
		}
		data := consecutive.payload
		if len(data) > remaining {
			data = data[:remaining]
		}
		payload = append(payload, data...)
		expectedSequence = (expectedSequence + 1) & 0x0f
		blockCount++

		if link.blockSize > 0 && blockCount == link.blockSize && len(payload) < total {
			if err := link.sendFlowControl(ctx, flowContinue); err != nil {
				return nil, err
			}
			blockCount = 0
		}
	}
	return payload, nil
}

func (link *Link) sendFlowControl(ctx context.Context, status flowStatus) error {
	frame, err := link.makeFrame([]byte{
		0x30 | byte(status),
		link.blockSize,
		link.separationTime,
	}, false)
	if err != nil {
		return err
	}
	return link.sendFrame(ctx, frame)
}
