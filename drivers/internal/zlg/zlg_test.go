package zlg

import (
	"testing"

	"github.com/tomrford/gocan"
)

func TestControllerEventTranslation(t *testing.T) {
	// USBCANFD lacks ReadChannelStatus (status == nil): chain lastState like
	// checkState through BusOff (+device overflow) -> Passive -> clean 0 poll.
	var last gocan.Event
	degraded := func() bool {
		return last.Kind == gocan.EventControllerState && last.ControllerState != gocan.ControllerActive
	}
	event, lost := controllerEvent(1, nil, errorBusOff|errorDeviceOverflow, false, degraded())
	if event.Kind != gocan.EventControllerState || event.ControllerState != gocan.ControllerBusOff || event.ErrorCountsKnown || !lost {
		t.Fatalf("FD bus-off+device overflow = %+v, lost=%v; want bus-off and lost", event, lost)
	}
	last = event
	event, lost = controllerEvent(1, nil, errorPassive, false, degraded())
	if event.Kind != gocan.EventControllerState || event.ControllerState != gocan.ControllerPassive || event.ErrorCountsKnown || lost {
		t.Fatalf("FD passive after bus-off = %+v, lost=%v; want ControllerPassive", event, lost)
	}
	last = event
	event, lost = controllerEvent(1, nil, 0, false, degraded())
	if event.Kind != gocan.EventControllerState || event.ControllerState != gocan.ControllerActive || event.ErrorCountsKnown || lost {
		t.Fatalf("FD clean recovery after passive = %+v, lost=%v; want ControllerActive with unknown counts", event, lost)
	}
}
