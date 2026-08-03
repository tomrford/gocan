package isotp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
)

// transmit sends one complete payload. Callers must hold the sending token, and
// the receiving token as well when the transmission is segmented, because
// waitFlowControl advances the link's receive position.
func (link *Link) transmit(ctx context.Context, transmission transmission) error {
	if err := link.sendFrame(ctx, transmission.firstFrame); err != nil {
		return err
	}
	if !transmission.multiFrame {
		return nil
	}

	sequence := uint8(1)
	for transmission.offset < len(transmission.payload) {
		control, err := link.waitFlowControl(ctx)
		if err != nil {
			return err
		}

		blockCount := uint8(0)
		for transmission.offset < len(transmission.payload) && (control.blockSize == 0 || blockCount < control.blockSize) {
			if err := waitContext(ctx, control.separationTime); err != nil {
				return err
			}
			capacity := link.transmitDataLength - 1
			end := min(transmission.offset+capacity, len(transmission.payload))
			data := make([]byte, 1+end-transmission.offset)
			data[0] = 0x20 | sequence
			copy(data[1:], transmission.payload[transmission.offset:end])
			frame, err := link.makeFrame(data, false)
			if err != nil {
				return err
			}
			if err := link.sendFrame(ctx, frame); err != nil {
				return err
			}
			transmission.offset = end
			sequence = (sequence + 1) & 0x0f
			blockCount++
		}
	}
	return nil
}

func (link *Link) waitFlowControl(ctx context.Context) (pdu, error) {
	// Counted as an int: a uint8 cannot exceed a limit of 255 and would silently
	// let a peer stall the transfer for ever.
	waitFrames := 0
	for {
		control, err := link.nextPDUWithTimeout(ctx, link.flowControlTimeout, ErrFlowControlTimeout)
		if err != nil {
			return pdu{}, err
		}
		if control.kind != frameFlowControl {
			return pdu{}, fmt.Errorf("%w: expected Flow Control, got frame type %#x", ErrProtocol, control.kind)
		}

		switch control.flowStatus {
		case flowContinue:
			return control, nil
		case flowWait:
			waitFrames++
			if waitFrames > int(link.waitFrameLimit) {
				return pdu{}, fmt.Errorf("%w: peer sent %d Wait frames", ErrWaitFrameLimit, waitFrames)
			}
		case flowOverflow:
			return pdu{}, ErrFlowControlOverflow
		}
	}
}

func (link *Link) sendFrame(ctx context.Context, frame gocan.Frame) error {
	// TODO: Decide a measured retry delay and limit for ErrTransmitQueueFull.
	// Retrying that explicitly rejected frame is protocol-safe because the bus
	// did not accept or record it. Any other mid-transfer Send error terminates
	// the transmission because restarting a PDU could corrupt peer state.
	return link.bus.Send(ctx, frame)
}

func (link *Link) nextPDUWithTimeout(ctx context.Context, timeout time.Duration, timeoutError error) (pdu, error) {
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	frame, err := link.nextFrame(waitContext)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return pdu{}, timeoutError
		}
		return pdu{}, err
	}
	return parseFrame(frame.Frame)
}

func waitContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
