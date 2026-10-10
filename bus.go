package gocan

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrBusClosed indicates an operation on a bus that has stopped.
	ErrBusClosed = errors.New("CAN bus is closed")
	// ErrReceiveOverrun indicates that a driver could not retain incoming
	// traffic before its bounded receive queue filled. A driver stops its bus
	// with this error instead of silently dropping frames.
	ErrReceiveOverrun = errors.New("CAN receive queue overrun")
	// ErrBusOff indicates that a controller stopped participating on the bus
	// after its transmit error counter exceeded the bus-off threshold.
	ErrBusOff = errors.New("CAN controller is bus-off")
	// ErrTransmitQueueFull indicates that a non-blocking native transmit queue
	// cannot currently accept another frame. The bus remains usable and callers
	// may retry under their own context.
	ErrTransmitQueueFull = errors.New("CAN transmit queue is full")
	// ErrHardwareDisconnected indicates that an adapter or channel disappeared
	// after it was opened. The bus stops; reconnecting hardware requires a new
	// Open call.
	ErrHardwareDisconnected = errors.New("CAN hardware disconnected")
	// ErrDriverUnavailable indicates that a driver's native vendor stack is
	// not installed on this host, as opposed to an installed stack failing.
	ErrDriverUnavailable = errors.New("CAN driver is not installed")
)

// Bus owns one logical CAN channel. Every accepted transmission and received
// frame is recorded in its non-nil Capture, which remains the same for its lifetime.
// Consumers receive traffic through Capture.
//
// Send is safe concurrently. A native send in progress cannot be revoked by
// cancellation: Send waits for its definite result and records acceptance before
// returning nil. Within a bus, transmission is recorded before its response.
// Capture timestamps describe host observation order, not wire order.
// Readiness waits do not hold up transmission.
//
// Send returns ErrTransmitQueueFull without waiting or recording acceptance.
// The package-level Send function provides bounded queue-full retries.
//
// Done closes after acquisition stops; Err then reports the background failure
// or nil after normal Close. ID is the one-based trace channel; Name is its label.
// Close is idempotent.
type Bus interface {
	ID() BusID
	Name() string
	Capture() *Capture
	Send(context.Context, Frame) error
	Done() <-chan struct{}
	Err() error
	Close() error
}

// Send hands frame to bus, retrying only ErrTransmitQueueFull. A zero
// retryTimeout makes one attempt; a positive value bounds waiting from the
// first rejection and returns the last rejection once spent. A negative value is
// invalid. The caller's earlier deadline, cancellation, or bus closure ends
// retries, joined to the rejection. Rejected frames are never recorded
// as accepted transmissions, and other errors return immediately.
//
// This waits for queue acceptance, not delivery on the wire. As with Bus.Send,
// a native call in progress must finish with a definite result even if its
// context expires. An accepted frame is never retried.
// If the first attempt returns the context's cancellation or deadline error,
// its custom cause is joined to that error, preserving errors.Is matching for both.
func Send(ctx context.Context, bus Bus, frame Frame, retryTimeout time.Duration) error {
	if retryTimeout < 0 {
		return errors.New("CAN transmit retry timeout must not be negative")
	}
	err := bus.Send(ctx, frame)
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) && context.Cause(ctx) != ctx.Err() {
		err = errors.Join(err, context.Cause(ctx))
	}
	if retryTimeout == 0 || !errors.Is(err, ErrTransmitQueueFull) {
		return err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, retryTimeout, ErrTransmitQueueFull)
	defer cancel()
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
		case <-bus.Done():
			cause := bus.Err()
			if cause == nil {
				cause = ErrBusClosed
			}
			return errors.Join(err, cause)
		case <-timer.C:
		}
		if cause := context.Cause(ctx); errors.Is(cause, ErrTransmitQueueFull) {
			return err
		} else if cause != nil {
			return errors.Join(err, cause)
		}
		nextErr := bus.Send(ctx, frame)
		if ctx.Err() != nil && errors.Is(nextErr, ctx.Err()) {
			continue
		}
		err = nextErr
		if !errors.Is(err, ErrTransmitQueueFull) {
			return err
		}
		timer.Reset(time.Millisecond)
	}
}
