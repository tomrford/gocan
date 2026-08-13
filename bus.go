package gocan

import (
	"context"
	"errors"
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
// Capture returns the non-nil capture that records this bus's traffic. It
// returns the same capture for the bus's lifetime. Done is closed after
// acquisition stops. Err then reports the background failure, or nil after a
// normal Close. ID is the one-based channel stored in captures and trace files;
// Name is its human-readable label. Close is idempotent.
//
// A full native transmit queue is reported as ErrTransmitQueueFull. Send does
// not wait for queue space or append a rejected transmission; callers decide
// whether and how to retry under their context.
type Bus interface {
	ID() BusID
	Name() string
	Capture() *Capture
	Send(context.Context, Frame) error
	Done() <-chan struct{}
	Err() error
	Close() error
}
