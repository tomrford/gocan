package virtual

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/conformance"
)

func TestConformance(t *testing.T) {
	open := func(t *testing.T, capture *gocan.Capture) (gocan.Bus, gocan.Bus) {
		var network Network
		a, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "conformance-a"})
		if err != nil {
			t.Fatalf("Open a: %v", err)
		}
		t.Cleanup(func() { _ = a.Close() })
		b, err := network.Open(context.Background(), capture, Config{ID: 2, Name: "conformance-b"})
		if err != nil {
			t.Fatalf("Open b: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return a, b
	}

	conformance.Run(t, open, conformance.Capabilities{
		FD:                     true,
		RemoteFrames:           true,
		ClassicalDLCAboveEight: true,
		InduceOverrun: func(t *testing.T, healthy, target gocan.Bus) {
			// Holding the state lock stalls target's receive loop, so its
			// queue fills from real traffic.
			stalled := target.(*Bus)
			stalled.stateMu.Lock()
			frame, err := gocan.NewFrame(0x7ff, nil, 0)
			if err != nil {
				stalled.stateMu.Unlock()
				t.Fatalf("NewFrame: %v", err)
			}
			for range receiveQueueCapacity + 2 {
				if err := healthy.Send(context.Background(), frame); err != nil {
					stalled.stateMu.Unlock()
					t.Fatalf("Send: %v", err)
				}
			}
			stalled.stateMu.Unlock()
			deadline := time.Now().Add(5 * time.Second)
			for {
				events, _, _ := target.Capture().BusEvents(target.ID(), gocan.Range{})
				if slices.ContainsFunc(slices.Collect(events), func(event gocan.Event) bool {
					return event.Kind == gocan.EventReceiveOverrun
				}) {
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("target reported no overrun")
				}
				time.Sleep(time.Millisecond)
			}
		},
		InduceBusOff: func(t *testing.T, target gocan.Bus) func() {
			bus := target.(*Bus)
			if err := bus.SetControllerState(gocan.ControllerBusOff); err != nil {
				t.Fatalf("SetControllerState: %v", err)
			}
			return func() {
				if err := bus.SetControllerState(gocan.ControllerActive); err != nil {
					t.Errorf("SetControllerState: %v", err)
				}
			}
		},
	})
}
