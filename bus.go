package gocan

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrBusClosed indicates an operation on a bus that has stopped.
	ErrBusClosed = errors.New("gocan: CAN bus is closed")
	// ErrBusOff indicates that a controller stopped participating on the bus
	// after its transmit error counter exceeded the bus-off threshold. Send
	// returns it for a frame the controller rejected while bus-off; the bus
	// remains open, reports the state as an EventControllerState, and recovers.
	ErrBusOff = errors.New("gocan: CAN controller is bus-off")
	// ErrTransmitQueueFull indicates that a non-blocking native transmit queue
	// cannot currently accept another frame. The bus remains usable and callers
	// may retry under their own context.
	ErrTransmitQueueFull = errors.New("gocan: CAN transmit queue is full")
	// ErrHardwareDisconnected indicates that an adapter or channel disappeared
	// after it was opened. The bus stops; reconnecting hardware requires a new
	// Open call.
	ErrHardwareDisconnected = errors.New("gocan: CAN hardware disconnected")
	// ErrDriverUnavailable indicates that a driver's native vendor stack is
	// not installed on this host, as opposed to an installed stack failing.
	ErrDriverUnavailable = errors.New("gocan: CAN driver is not installed")
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
// Send returns ErrTransmitQueueFull or ErrBusOff without waiting or recording
// acceptance. The package-level Send function provides bounded retries.
//
// Context returns the same non-nil context on every call. It is cancelled once
// acquisition stops, with the background failure (or ErrBusClosed after normal
// Close) as its cause. ID is the one-based trace channel; Name is its label.
// Close is idempotent.
type Bus interface {
	ID() BusID
	Name() string
	Capture() *Capture
	Send(context.Context, Frame) error
	Context() context.Context
	Close() error
}

// Send hands frame to bus, retrying only ErrTransmitQueueFull and ErrBusOff.
// A zero retryTimeout makes one attempt; a positive value bounds waiting from
// the first rejection and returns the last rejection once spent. A negative
// value is invalid. The caller's earlier deadline, cancellation, or bus closure
// ends retries, joined to the rejection. Rejected frames are never recorded
// as accepted transmissions, and other errors return immediately.
//
// This waits for queue acceptance, not delivery on the wire. As with Bus.Send,
// a native call in progress must finish with a definite result even if its
// context expires. An accepted frame is never retried.
// If the first attempt returns the context's cancellation or deadline error,
// its custom cause is joined to that error, preserving errors.Is matching for both.
func Send(ctx context.Context, bus Bus, frame Frame, retryTimeout time.Duration) error {
	if retryTimeout < 0 {
		return errors.New("gocan: transmit retry timeout must not be negative")
	}
	err := bus.Send(ctx, frame)
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) && context.Cause(ctx) != ctx.Err() {
		err = errors.Join(err, context.Cause(ctx))
	}
	if retryTimeout == 0 || !retryable(err) {
		return err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, retryTimeout, errRetrySpent)
	defer cancel()
	timer := time.NewTimer(retryInterval(err))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
		case <-bus.Context().Done():
			cause := context.Cause(bus.Context())
			if cause == nil {
				cause = ErrBusClosed
			}
			return errors.Join(err, cause)
		case <-timer.C:
		}
		if cause := context.Cause(ctx); cause == errRetrySpent {
			return err
		} else if cause != nil {
			return errors.Join(err, cause)
		}
		nextErr := bus.Send(ctx, frame)
		if ctx.Err() != nil && errors.Is(nextErr, ctx.Err()) {
			continue
		}
		err = nextErr
		if !retryable(err) {
			return err
		}
		timer.Reset(retryInterval(err))
	}
}

var errRetrySpent = errors.New("gocan: transmit retry timeout spent")

func retryable(err error) bool {
	return errors.Is(err, ErrTransmitQueueFull) || errors.Is(err, ErrBusOff)
}

func retryInterval(err error) time.Duration {
	if errors.Is(err, ErrBusOff) {
		// Recovery takes at least 128 x 11 recessive bit times, and PCAN-USB
		// still rejected frames 100 ms after reporting bus-off (November 2026).
		return 10 * time.Millisecond
	}
	return time.Millisecond
}
