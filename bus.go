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

// Bus owns one logical CAN channel. It accepts frames for transmission and
// contributes every accepted transmission and received frame to the Capture
// supplied when the concrete driver was opened.
//
// Bus has no receive method. Consumers observe traffic through that Capture,
// so one native receive loop can preserve every frame without dividing traffic
// among competing readers.
//
// Send is safe to call concurrently. Concrete drivers serialise calls as
// required by their native API. Once a send has been handed to that API,
// context cancellation cannot revoke it: Send waits for the definite native
// result and records an accepted transmission before returning nil.
//
// Within a bus, each native read or write and its capture append are serialised:
// an accepted transmission is recorded before the next native read. Drivers use
// Capture.RecordFrame and RecordEvent, whose shared clock follows capture append
// order across buses. Timestamps describe host observation order, not the order
// of buffered traffic on the wire.
// Readiness waits do not hold up transmission.
//
// Capture returns the non-nil capture that records this bus's traffic. It
// returns the same capture for the bus's lifetime. Done is closed after
// acquisition stops. Err then reports the background failure, or nil after a
// normal Close. ID is the one-based channel stored in captures and trace files;
// Name is its human-readable label. Close is idempotent.
//
// A full native transmit queue is reported as ErrTransmitQueueFull. Send does
// not wait for queue space or append a rejected transmission; callers decide
// whether and how to retry under their context.
// The package-level Send function provides bounded queue-full retries.
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
func Send(ctx context.Context, bus Bus, frame Frame, retryTimeout time.Duration) error {
	if retryTimeout < 0 {
		return errors.New("CAN transmit retry timeout must not be negative")
	}
	err := bus.Send(ctx, frame)
	if retryTimeout == 0 || !errors.Is(err, ErrTransmitQueueFull) {
		return err
	}
	// The budget's cause keeps its expiry distinct from the caller's deadline.
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
		// The timer can win a race with expiry. Report the rejection rather than
		// a bare context error from the driver.
		if cause := context.Cause(ctx); errors.Is(cause, ErrTransmitQueueFull) {
			return err
		} else if cause != nil {
			return errors.Join(err, cause)
		}
		err = bus.Send(ctx, frame)
		if !errors.Is(err, ErrTransmitQueueFull) {
			return err
		}
		timer.Reset(time.Millisecond)
	}
}
