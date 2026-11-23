package transport

import (
	"context"

	"github.com/tomrford/gocan"
)

// StopError returns err, the failure a stopped bus reported, or
// gocan.ErrBusClosed when it stopped normally.
func StopError(err error) error {
	if err != nil {
		return err
	}
	return gocan.ErrBusClosed
}

// BusError reports why bus stopped. Call it once bus.Done has closed.
func BusError(bus gocan.Bus) error {
	return StopError(bus.Err())
}

// BusContext returns a child of parent that is also cancelled with BusError
// once bus stops. A bus that has already stopped cancels it before return.
// Otherwise one goroutine watches bus until either ends, so cancel the context
// when the operation it scopes finishes.
//
// The watcher is started by the caller rather than shared across operations:
// a Bus offers only a channel, and a long-lived watcher would outlive values
// that have no Close and would cancel contexts across testing/synctest bubbles.
func BusContext(parent context.Context, bus gocan.Bus) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	if ctx.Err() != nil {
		return ctx, cancel
	}
	select {
	case <-bus.Done():
		cancel(BusError(bus))
		return ctx, cancel
	default:
	}
	go func() {
		select {
		case <-bus.Done():
			cancel(BusError(bus))
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// Join returns a child of parent that is also cancelled with the cause of
// other once other ends. An already-ended other cancels it before return;
// otherwise no goroutine starts unless other ends first. Calling cancel
// detaches other and cancels the child with cause.
func Join(parent, other context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	if ctx.Err() != nil {
		return ctx, cancel
	}
	if other.Err() != nil {
		cancel(context.Cause(other))
		return ctx, cancel
	}
	stop := context.AfterFunc(other, func() { cancel(context.Cause(other)) })
	return ctx, func(cause error) {
		stop()
		cancel(cause)
	}
}
