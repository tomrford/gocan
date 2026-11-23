package driverstate

import (
	"context"
	"errors"
	"testing"

	"github.com/tomrford/gocan"
)

func TestLifecycleRetainsLateFirstFailure(t *testing.T) {
	wakes := 0
	lifecycle := New(func() { wakes++ })

	lifecycle.Stop(nil)
	failure := errors.New("receive failed during shutdown")
	lifecycle.Stop(failure)
	lifecycle.Stop(errors.New("later failure"))

	if wakes != 1 {
		t.Fatalf("wake called %d times, want 1", wakes)
	}
	if !errors.Is(lifecycle.Err(), failure) {
		t.Fatalf("Err() = %v, want %v", lifecycle.Err(), failure)
	}
	if !errors.Is(lifecycle.OperationError(), failure) {
		t.Fatalf("OperationError() = %v, want %v", lifecycle.OperationError(), failure)
	}
	select {
	case <-lifecycle.StopSignal():
	default:
		t.Fatal("StopSignal is open after Stop")
	}

	lifecycle.MarkDone()
	select {
	case <-lifecycle.Context().Done():
	default:
		t.Fatal("Context is open after MarkDone")
	}
	if cause := context.Cause(lifecycle.Context()); !errors.Is(cause, failure) {
		t.Fatalf("Context cause = %v, want %v", cause, failure)
	}

	// A late Stop callback after MarkDone (e.g. a device-removal callback
	// copied before unsubscribe) must not make Err() disagree with Context().
	clean := New(nil)
	clean.Stop(nil)
	clean.MarkDone()
	clean.Stop(gocan.ErrHardwareDisconnected)
	if err := clean.Err(); err != nil {
		t.Fatalf("Err() after post-MarkDone Stop = %v, want nil", err)
	}
	if cause := context.Cause(clean.Context()); !errors.Is(cause, gocan.ErrBusClosed) {
		t.Fatalf("Context cause after post-MarkDone Stop = %v, want ErrBusClosed", cause)
	}
}
