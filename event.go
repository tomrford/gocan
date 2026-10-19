package gocan

import (
	"errors"
	"fmt"
	"time"
)

// EventKind identifies a non-frame observation stored in a Capture.
type EventKind uint8

const (
	// EventControllerState reports the CAN controller's operating state.
	EventControllerState EventKind = iota + 1
	// EventErrorFrame reports that a driver or controller observed a CAN error
	// condition.
	EventErrorFrame
	// EventReceiveOverrun reports that incoming traffic was lost.
	EventReceiveOverrun
)

// String returns the kind's name, or "EventKind(n)" for an undefined value.
func (kind EventKind) String() string {
	switch kind {
	case EventControllerState:
		return "controller state"
	case EventErrorFrame:
		return "error frame"
	case EventReceiveOverrun:
		return "receive overrun"
	default:
		return fmt.Sprintf("EventKind(%d)", uint8(kind))
	}
}

// ControllerState is the operating state of a CAN controller.
type ControllerState uint8

const (
	// ControllerActive indicates ordinary error-active operation.
	ControllerActive ControllerState = iota + 1
	// ControllerWarning indicates that an error counter reached its warning
	// threshold.
	ControllerWarning
	// ControllerPassive indicates error-passive operation.
	ControllerPassive
	// ControllerBusOff indicates that the controller entered the bus-off state.
	ControllerBusOff
)

// String returns the state's name as the trace formats spell it, or
// "ControllerState(n)" for an undefined value.
func (state ControllerState) String() string {
	switch state {
	case ControllerActive:
		return "error active"
	case ControllerWarning:
		return "error warning"
	case ControllerPassive:
		return "error passive"
	case ControllerBusOff:
		return "bus off"
	default:
		return fmt.Sprintf("ControllerState(%d)", uint8(state))
	}
}

// Event is one non-frame bus observation and its capture metadata.
//
// ControllerState, TXErrorCount, RXErrorCount, and ErrorCountsKnown are set
// only for EventControllerState. ErrorCountsKnown distinguishes unavailable
// controller counters from counters whose reported value is zero.
type Event struct {
	Bus              BusID
	Timestamp        time.Time
	Kind             EventKind
	ControllerState  ControllerState
	TXErrorCount     uint8
	RXErrorCount     uint8
	ErrorCountsKnown bool
}

// NewControllerStateEvent constructs and validates a controller-state event.
func NewControllerStateEvent(
	bus BusID,
	timestamp time.Time,
	state ControllerState,
	txErrorCount uint8,
	rxErrorCount uint8,
	errorCountsKnown bool,
) (Event, error) {
	event := Event{
		Bus:              bus,
		Timestamp:        timestamp,
		Kind:             EventControllerState,
		ControllerState:  state,
		TXErrorCount:     txErrorCount,
		RXErrorCount:     rxErrorCount,
		ErrorCountsKnown: errorCountsKnown,
	}
	return event, event.Validate()
}

// NewErrorFrameEvent constructs and validates an error-frame event.
func NewErrorFrameEvent(bus BusID, timestamp time.Time) (Event, error) {
	event := Event{Bus: bus, Timestamp: timestamp, Kind: EventErrorFrame}
	return event, event.Validate()
}

// NewReceiveOverrunEvent constructs and validates a receive-overrun event.
func NewReceiveOverrunEvent(bus BusID, timestamp time.Time) (Event, error) {
	event := Event{Bus: bus, Timestamp: timestamp, Kind: EventReceiveOverrun}
	return event, event.Validate()
}

// Validate reports whether event is suitable for capture.
func (event Event) Validate() error {
	if event.Bus == 0 {
		return errors.New("event has no bus")
	}
	if event.Timestamp.IsZero() {
		return errors.New("event has no timestamp")
	}

	switch event.Kind {
	case EventControllerState:
		if event.ControllerState < ControllerActive || event.ControllerState > ControllerBusOff {
			return fmt.Errorf("event has invalid controller state %d", event.ControllerState)
		}
		if !event.ErrorCountsKnown && (event.TXErrorCount != 0 || event.RXErrorCount != 0) {
			return errors.New("event has controller error counts marked unavailable")
		}
	case EventErrorFrame, EventReceiveOverrun:
		if event.ControllerState != 0 || event.TXErrorCount != 0 || event.RXErrorCount != 0 || event.ErrorCountsKnown {
			return fmt.Errorf("event kind %d has controller-state details", event.Kind)
		}
	default:
		return fmt.Errorf("event has invalid kind %d", event.Kind)
	}
	return nil
}

// String returns the event's bus, kind, and controller details when they are
// known, for example "bus 1 controller state bus off (tx 12 rx 3)". Timestamp is
// not included; format it separately when a log needs it.
func (event Event) String() string {
	if event.Kind == EventControllerState {
		if event.ErrorCountsKnown {
			return fmt.Sprintf("bus %d controller state %s (tx %d rx %d)",
				event.Bus, event.ControllerState, event.TXErrorCount, event.RXErrorCount)
		}
		return fmt.Sprintf("bus %d controller state %s", event.Bus, event.ControllerState)
	}
	return fmt.Sprintf("bus %d %s", event.Bus, event.Kind)
}
