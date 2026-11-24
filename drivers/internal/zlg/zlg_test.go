package zlg

import (
	"testing"

	"github.com/tomrford/gocan"
)

func TestControllerEventTranslation(t *testing.T) {
	// USBCANFD lacks ReadChannelStatus (status == nil): an idle poll produces
	// no state event, bus-off emits ControllerBusOff alongside any overrun, a
	// repeated bus-off stays bus-off, and the next clean poll emits recovery to
	// ControllerActive with unknown counts.
	if event, lost := controllerEvent(1, nil, 0, false, false); event.Kind != 0 || lost {
		t.Fatalf("FD idle = %+v, lost=%v; want zero event", event, lost)
	}
	if event, lost := controllerEvent(1, nil, errorBusOff|errorBufferOverflow, false, false); event.Kind != gocan.EventControllerState ||
		event.ControllerState != gocan.ControllerBusOff || event.ErrorCountsKnown || !lost {
		t.Fatalf("FD bus-off+overflow = %+v, lost=%v; want bus-off and lost", event, lost)
	}
	if event, lost := controllerEvent(1, nil, errorBusOff, false, true); event.Kind != gocan.EventControllerState ||
		event.ControllerState != gocan.ControllerBusOff || event.ErrorCountsKnown || lost {
		t.Fatalf("FD repeated bus-off = %+v, lost=%v; want bus-off", event, lost)
	}
	if event, lost := controllerEvent(1, nil, 0, false, true); event.Kind != gocan.EventControllerState ||
		event.ControllerState != gocan.ControllerActive || event.ErrorCountsKnown || lost {
		t.Fatalf("FD recovery = %+v, lost=%v; want ControllerActive with unknown counts", event, lost)
	}

	// Classic USBCAN reports real SJA1000 status and error counts.
	busOffStatus := &channelStatus{status: sja1000BusStatus, txErrorCount: 255, rxErrorCount: 4}
	if event, lost := controllerEvent(2, busOffStatus, errorControllerOverflow, true, false); event.Kind != gocan.EventControllerState ||
		event.ControllerState != gocan.ControllerBusOff || !event.ErrorCountsKnown ||
		event.TXErrorCount != 255 || event.RXErrorCount != 4 || !lost {
		t.Fatalf("classic bus-off = %+v, lost=%v", event, lost)
	}
	recovered := &channelStatus{txErrorCount: 10, rxErrorCount: 2}
	if event, lost := controllerEvent(2, recovered, 0, true, true); event.Kind != gocan.EventControllerState ||
		event.ControllerState != gocan.ControllerActive || !event.ErrorCountsKnown ||
		event.TXErrorCount != 10 || event.RXErrorCount != 2 || lost {
		t.Fatalf("classic recovery = %+v, lost=%v", event, lost)
	}
}
