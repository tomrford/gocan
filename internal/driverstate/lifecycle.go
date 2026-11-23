package driverstate

import (
	"context"
	"sync"

	"github.com/tomrford/gocan/internal/transport"
)

// Lifecycle coordinates a driver's receive goroutine with concurrent callers.
// The first non-nil failure before MarkDone is retained even when it arrives
// after Stop closed the stop signal.
type Lifecycle struct {
	stopOnce sync.Once
	stop     chan struct{}
	ctx      context.Context
	cancel   context.CancelCauseFunc
	wake     func()

	errMu sync.RWMutex
	err   error
	done  bool
}

// New returns an active lifecycle. wake must unblock the driver's native
// receive wait; it runs at most once.
func New(wake func()) *Lifecycle {
	ctx, cancel := context.WithCancelCause(context.Background())
	// Eagerly initialize the done channel so it belongs to the creator's
	// testing/synctest bubble rather than whichever goroutine first selects on it.
	_ = ctx.Done()
	return &Lifecycle{
		stop:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
		wake:   wake,
	}
}

func (lifecycle *Lifecycle) Stop(err error) {
	if err != nil {
		lifecycle.errMu.Lock()
		if !lifecycle.done && lifecycle.err == nil {
			lifecycle.err = err
		}
		lifecycle.errMu.Unlock()
	}
	lifecycle.stopOnce.Do(func() {
		close(lifecycle.stop)
		if lifecycle.wake != nil {
			lifecycle.wake()
		}
	})
}

func (lifecycle *Lifecycle) StopSignal() <-chan struct{} { return lifecycle.stop }

// Context returns a context that is cancelled with OperationError once
// MarkDone runs.
func (lifecycle *Lifecycle) Context() context.Context { return lifecycle.ctx }

func (lifecycle *Lifecycle) Done() <-chan struct{} { return lifecycle.ctx.Done() }

// MarkDone reports that native cleanup is complete, freezes Err, and cancels
// Context with OperationError. The receive goroutine must call it exactly once.
func (lifecycle *Lifecycle) MarkDone() {
	lifecycle.errMu.Lock()
	lifecycle.done = true
	cause := transport.StopError(lifecycle.err)
	lifecycle.errMu.Unlock()
	lifecycle.cancel(cause)
}

func (lifecycle *Lifecycle) Err() error {
	lifecycle.errMu.RLock()
	defer lifecycle.errMu.RUnlock()
	return lifecycle.err
}

// OperationError returns the background failure, or ErrBusClosed after a
// normal stop.
func (lifecycle *Lifecycle) OperationError() error {
	return transport.StopError(lifecycle.Err())
}
