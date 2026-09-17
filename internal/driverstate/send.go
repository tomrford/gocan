package driverstate

import (
	"context"
	"errors"
	"time"

	"github.com/tomrford/gocan"
)

// Send implements Bus.Send around one driver attempt. The attempt must return
// a definite outcome and release its I/O locks before returning, so waiting for
// queue space does not block receive progress or other senders.
func Send(ctx context.Context, bus gocan.Bus, frame gocan.Frame, retryTimeout time.Duration, attempt func(context.Context, gocan.Frame) error) error {
	if retryTimeout < 0 {
		return errors.New("CAN transmit retry timeout must not be negative")
	}
	err := attempt(ctx, frame)
	if retryTimeout == 0 || !errors.Is(err, gocan.ErrTransmitQueueFull) {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, retryTimeout)
	defer cancel()
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.Join(err, context.Cause(ctx))
		case <-bus.Done():
			cause := bus.Err()
			if cause == nil {
				cause = gocan.ErrBusClosed
			}
			return errors.Join(err, cause)
		case <-timer.C:
		}
		if cause := context.Cause(ctx); cause != nil {
			return errors.Join(err, cause)
		}
		err = attempt(ctx, frame)
		if !errors.Is(err, gocan.ErrTransmitQueueFull) {
			return err
		}
		timer.Reset(time.Millisecond)
	}
}

// SentCursor locates the accepted transmission after a successful Send. The
// caller must exclusively own the transmit identifier and retain after until
// this lookup completes. Native TX-before-RX ordering preserves even a reply
// captured before Send returned, while excluding traffic captured during retries.
func SentCursor(bus gocan.Bus, frame gocan.Frame, after gocan.Cursor) (gocan.Cursor, error) {
	// Next searches retained records before checking cancellation. Never wait:
	// a missing accepted transmission means capture loss, including Clear from
	// an initially empty capture whose zero cursor cannot become invalid.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cursor, err := bus.Capture().Next(ctx, gocan.FrameKey{
		Bus: bus.ID(), ID: frame.ID, Extended: frame.Flags.Has(gocan.FrameExtended), Direction: gocan.DirectionTransmit,
	}, after)
	if errors.Is(err, context.Canceled) {
		err = gocan.ErrCursorOutOfRange
	}
	return cursor, err
}
