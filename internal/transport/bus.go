package transport

import (
	"context"
	"sync"

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

// BusContext returns a context cancelled with BusError once bus stops. One
// goroutine watches bus until it stops or release is called; release stops
// watching without cancelling the context. Derive per-operation contexts with
// Join so that operations start no goroutines of their own.
func BusContext(bus gocan.Bus) (ctx context.Context, release func()) {
	ctx, cancel := context.WithCancelCause(context.Background())
	quit := make(chan struct{})
	go func() {
		select {
		case <-bus.Done():
			cancel(BusError(bus))
		case <-quit:
		}
	}()
	var once sync.Once
	return ctx, func() { once.Do(func() { close(quit) }) }
}

// Join returns a child of parent that is also cancelled with the cause of
// other once other ends. It starts no goroutine until other ends. Calling
// cancel detaches other and cancels the child with cause.
func Join(parent, other context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	stop := context.AfterFunc(other, func() { cancel(context.Cause(other)) })
	return ctx, func(cause error) {
		stop()
		cancel(cause)
	}
}
