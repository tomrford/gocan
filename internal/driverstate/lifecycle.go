// Package driverstate contains lifecycle state shared by native CAN drivers.
package driverstate

import (
	"sync"

	"github.com/tomrford/gocan"
)

// Lifecycle coordinates a driver's receive goroutine with concurrent callers.
// The first non-nil failure is retained even when it arrives after Stop closed
// the stop signal.
type Lifecycle struct {
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	wake     func()

	errMu sync.RWMutex
	err   error
}

// New returns an active lifecycle. wake must unblock the driver's native
// receive wait; it runs at most once.
func New(wake func()) *Lifecycle {
	return &Lifecycle{
		stop: make(chan struct{}),
		done: make(chan struct{}),
		wake: wake,
	}
}

// Stop records the first failure, closes StopSignal, and wakes native receive.
func (lifecycle *Lifecycle) Stop(err error) {
	if err != nil {
		lifecycle.errMu.Lock()
		if lifecycle.err == nil {
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

// StopSignal is closed when acquisition should stop.
func (lifecycle *Lifecycle) StopSignal() <-chan struct{} { return lifecycle.stop }

// Done is closed after the driver finishes native cleanup.
func (lifecycle *Lifecycle) Done() <-chan struct{} { return lifecycle.done }

// MarkDone reports that native cleanup is complete. The receive goroutine must
// call it exactly once.
func (lifecycle *Lifecycle) MarkDone() { close(lifecycle.done) }

// Err returns the first background failure, if any.
func (lifecycle *Lifecycle) Err() error {
	lifecycle.errMu.RLock()
	defer lifecycle.errMu.RUnlock()
	return lifecycle.err
}

// OperationError returns the background failure, or ErrBusClosed after a
// normal stop.
func (lifecycle *Lifecycle) OperationError() error {
	if err := lifecycle.Err(); err != nil {
		return err
	}
	return gocan.ErrBusClosed
}
