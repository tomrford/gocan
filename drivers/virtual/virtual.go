// Package virtual provides an in-process CAN network for development and
// tests that do not require physical hardware.
package virtual

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tomrford/gocan"
)

const receiveQueueCapacity = 1024

// Config selects one endpoint on a Network.
type Config struct {
	// ID is the one-based trace channel assigned to the bus.
	ID gocan.BusID
	// Name is the human-readable name of the bus.
	Name               string
	ReceiveOwnMessages bool
}

// Network connects virtual CAN buses. The zero Network is ready for use.
type Network struct {
	mu    sync.RWMutex
	buses map[gocan.BusID]*Bus
}

// Open creates and starts one virtual bus. Every bus opened on the same
// Network can exchange frames. Capture may be shared by any number of buses.
// Context controls opening only; canceling it after Open returns does not stop
// the bus.
func (network *Network) Open(ctx context.Context, capture *gocan.Capture, config Config) (*Bus, error) {
	if capture == nil {
		return nil, errors.New("virtual CAN bus requires a capture")
	}
	if config.ID == 0 {
		return nil, errors.New("virtual CAN bus requires an ID")
	}
	if config.Name == "" {
		return nil, errors.New("virtual CAN bus requires a name")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	network.mu.Lock()
	defer network.mu.Unlock()

	if network.buses == nil {
		network.buses = make(map[gocan.BusID]*Bus)
	}
	if _, exists := network.buses[config.ID]; exists {
		return nil, fmt.Errorf("virtual CAN bus ID %d is already open", config.ID)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	bus := &Bus{
		id:                 config.ID,
		name:               config.Name,
		capture:            capture,
		network:            network,
		receiveOwnMessages: config.ReceiveOwnMessages,
		sends:              make(chan sendRequest),
		incoming:           make(chan gocan.Frame, receiveQueueCapacity),
		overrun:            make(chan struct{}, 1),
		stop:               make(chan struct{}),
		done:               make(chan struct{}),
	}
	network.buses[config.ID] = bus
	go bus.run()
	return bus, nil
}

type sendRequest struct {
	frame  gocan.Frame
	result chan error
}

// Bus is one endpoint on a virtual Network.
type Bus struct {
	id                 gocan.BusID
	name               string
	capture            *gocan.Capture
	network            *Network
	receiveOwnMessages bool

	sends    chan sendRequest
	incoming chan gocan.Frame
	overrun  chan struct{}
	stop     chan struct{}
	done     chan struct{}

	closeOnce sync.Once
	stateMu   sync.Mutex
	closing   bool
	busOff    atomic.Bool
	errMu     sync.RWMutex
	err       error
}

var _ gocan.Bus = (*Bus)(nil)

// ID returns the identity used for this bus in its Capture.
func (bus *Bus) ID() gocan.BusID {
	return bus.id
}

// Name returns the human-readable name of this bus.
func (bus *Bus) Name() string {
	return bus.name
}

// Capture returns the capture that records this bus's traffic.
func (bus *Bus) Capture() *gocan.Capture {
	return bus.capture
}

// Send hands frame to the virtual network and records the accepted
// transmission before returning. It returns gocan.ErrBusOff while the endpoint
// is bus-off.
func (bus *Bus) Send(ctx context.Context, frame gocan.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}

	request := sendRequest{
		frame:  frame,
		result: make(chan error, 1),
	}
	if err := bus.sendable(); err != nil {
		return err
	}

	select {
	case bus.sends <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-bus.done:
		return bus.operationError()
	}

	select {
	case err := <-request.result:
		return err
	case <-bus.done:
		select {
		case err := <-request.result:
			return err
		default:
			return bus.operationError()
		}
	}
}

// SetControllerState records state as this endpoint's controller state, so
// tests can drive bus-off handling. While it is gocan.ControllerBusOff the
// endpoint neither transmits nor receives.
func (bus *Bus) SetControllerState(state gocan.ControllerState) error {
	bus.stateMu.Lock()
	defer bus.stateMu.Unlock()
	if bus.closing {
		return gocan.ErrBusClosed
	}
	select {
	case <-bus.done:
		return bus.operationError()
	default:
	}
	err := bus.capture.RecordEvent(gocan.Event{Bus: bus.id, Kind: gocan.EventControllerState, ControllerState: state})
	if err == nil {
		bus.busOff.Store(state == gocan.ControllerBusOff)
	}
	return err
}

// Done is closed when this bus stops.
func (bus *Bus) Done() <-chan struct{} {
	return bus.done
}

// Err returns the background failure that stopped this bus, if any.
func (bus *Bus) Err() error {
	bus.errMu.RLock()
	defer bus.errMu.RUnlock()
	return bus.err
}

// Close stops the bus. It is safe to call more than once.
func (bus *Bus) Close() error {
	bus.stateMu.Lock()
	bus.closing = true
	bus.closeOnce.Do(func() {
		close(bus.stop)
	})
	bus.stateMu.Unlock()
	<-bus.done
	return nil
}

func (bus *Bus) run() {
	var runErr error
	defer func() {
		bus.errMu.Lock()
		bus.err = runErr
		bus.errMu.Unlock()
		bus.network.remove(bus)
		close(bus.done)
	}()

	for {
		select {
		case request := <-bus.sends:
			bus.stateMu.Lock()
			err := bus.sendableLocked()
			if err == nil {
				err = bus.capture.RecordFrame(gocan.FrameEvent{
					Bus:       bus.id,
					Direction: gocan.DirectionTransmit,
					Frame:     request.frame,
				})
			}
			bus.stateMu.Unlock()
			if err == nil {
				bus.network.broadcast(bus, request.frame)
			}
			request.result <- err

		case received := <-bus.incoming:
			bus.stateMu.Lock()
			if bus.closing || bus.busOff.Load() {
				bus.stateMu.Unlock()
				continue
			}
			if err := bus.capture.RecordFrame(gocan.FrameEvent{
				Bus:       bus.id,
				Direction: gocan.DirectionReceive,
				Frame:     received,
			}); err != nil {
				bus.stateMu.Unlock()
				runErr = err
				return
			}
			bus.stateMu.Unlock()

		case <-bus.overrun:
			if err := bus.capture.RecordEvent(gocan.Event{Bus: bus.id, Kind: gocan.EventReceiveOverrun}); err != nil {
				runErr = err
				return
			}

		case <-bus.stop:
			return
		}
	}
}

func (bus *Bus) sendable() error {
	bus.stateMu.Lock()
	defer bus.stateMu.Unlock()
	return bus.sendableLocked()
}

func (bus *Bus) sendableLocked() error {
	if bus.closing {
		return gocan.ErrBusClosed
	}
	if bus.busOff.Load() {
		return fmt.Errorf("%w: virtual bus %d", gocan.ErrBusOff, bus.id)
	}
	return nil
}

func (bus *Bus) operationError() error {
	if err := bus.Err(); err != nil {
		return err
	}
	return gocan.ErrBusClosed
}

func (network *Network) broadcast(source *Bus, received gocan.Frame) {
	network.mu.RLock()
	defer network.mu.RUnlock()

	for _, destination := range network.buses {
		if destination == source && !source.receiveOwnMessages {
			continue
		}
		if destination.busOff.Load() {
			continue
		}
		select {
		case destination.incoming <- received:
		case <-destination.done:
		default:
			// The frame is lost; the destination reports one overrun per burst.
			select {
			case destination.overrun <- struct{}{}:
			default:
			}
		}
	}
}

func (network *Network) remove(bus *Bus) {
	network.mu.Lock()
	defer network.mu.Unlock()

	if network.buses[bus.id] == bus {
		delete(network.buses, bus.id)
	}
}
