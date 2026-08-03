package driverstate

import (
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
	case <-lifecycle.Done():
	default:
		t.Fatal("Done is open after MarkDone")
	}
}

func TestLifecycleNormalStopReportsBusClosed(t *testing.T) {
	lifecycle := New(nil)
	lifecycle.Stop(nil)
	if !errors.Is(lifecycle.OperationError(), gocan.ErrBusClosed) {
		t.Fatalf("OperationError() = %v, want ErrBusClosed", lifecycle.OperationError())
	}
}
