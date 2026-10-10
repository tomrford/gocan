package transport

import (
	"context"
	"errors"

	"github.com/tomrford/gocan"
)

// SentCursor locates the accepted transmission after a successful Send. The
// caller must exclusively own the transmit identifier and retain after until
// this lookup completes. Recording the accepted TX before its response preserves
// even a reply captured before Send returned, while excluding traffic captured
// during retries. This cursor does not exclude traffic still buffered in a driver.
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
