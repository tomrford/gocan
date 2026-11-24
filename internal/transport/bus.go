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

// BusError reports why bus stopped. Call it once bus.Context is done.
func BusError(bus gocan.Bus) error {
	if cause := context.Cause(bus.Context()); cause != nil {
		return cause
	}
	return gocan.ErrBusClosed
}

// BusContext returns a child of parent that is also cancelled with BusError
// once bus stops. An already-stopped bus or already-ended parent cancels
// synchronously before return; otherwise, for standard-library bus contexts, no
// goroutine starts unless bus stops before cancel is called. Callers must
// invoke the returned cancel function to detach from bus even after parent
// ends. Under testing/synctest, open and stop bus in the same bubble as
// operations joined to it.
func BusContext(parent context.Context, bus gocan.Bus) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	if ctx.Err() != nil {
		return ctx, cancel
	}
	busCtx := bus.Context()
	if busCtx.Err() != nil {
		cancel(BusError(bus))
		return ctx, cancel
	}
	stop := context.AfterFunc(busCtx, func() { cancel(BusError(bus)) })
	return ctx, func(cause error) {
		stop()
		cancel(cause)
	}
}

// Join returns a child of parent that is also cancelled with the cause of
// other once other ends. An already-ended other cancels it before return;
// otherwise, for standard-library contexts, no goroutine starts unless other
// ends before cancel is called. Callers must invoke the returned cancel
// function to detach other even after parent ends.
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
