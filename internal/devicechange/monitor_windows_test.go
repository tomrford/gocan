//go:build windows

package devicechange

import (
	"errors"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

type fakeInstanceWatcher struct {
	events chan Event
	lost   bool
	closed chan struct{}
}

func newFakeInstanceWatcher() *fakeInstanceWatcher {
	return &fakeInstanceWatcher{
		events: make(chan Event, 1),
		closed: make(chan struct{}),
	}
}

func (watcher *fakeInstanceWatcher) Events() <-chan Event { return watcher.events }
func (watcher *fakeInstanceWatcher) Lost() bool {
	lost := watcher.lost
	watcher.lost = false
	return lost
}
func (watcher *fakeInstanceWatcher) Close() error {
	close(watcher.closed)
	return nil
}

type stoppedBus struct{ stopped chan error }

func newStoppedBus() *stoppedBus { return &stoppedBus{stopped: make(chan error, 1)} }

func subscribe(t *testing.T, monitor *Monitor, bus *stoppedBus) *Subscription {
	t.Helper()
	hold, err := monitor.Hold()
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	subscription, err := hold.Bind(func(err error) { bus.stopped <- err })
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return subscription
}

func TestMonitorStopsAllSubscribersOnRemoval(t *testing.T) {
	watcher := newFakeInstanceWatcher()
	monitor := newMonitor("PEAK USB", func() (instanceWatcher, error) {
		return watcher, nil
	})
	first := newStoppedBus()
	second := newStoppedBus()
	firstSubscription := subscribe(t, monitor, first)
	defer firstSubscription.Cancel()
	secondSubscription := subscribe(t, monitor, second)
	defer secondSubscription.Cancel()

	staleHold, err := monitor.Hold()
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	watcher.events <- Event{InstanceID: `USB\VID_0C72&PID_000C\example`}
	for index, bus := range []*stoppedBus{first, second} {
		select {
		case err := <-bus.stopped:
			if !errors.Is(err, gocan.ErrHardwareDisconnected) {
				t.Fatalf("bus %d stopped with %v", index, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("bus %d did not stop", index)
		}
	}
	if _, err := staleHold.Bind(func(error) {}); !errors.Is(err, gocan.ErrHardwareDisconnected) {
		t.Fatalf("Bind across a removal = %v, want ErrHardwareDisconnected", err)
	}

	// Idle shutdown after both cancels proves the failed Bind returned its
	// hold; a leaked hold would keep the watcher registered forever.
	if err := firstSubscription.Cancel(); err != nil {
		t.Fatalf("Cancel first: %v", err)
	}
	if err := secondSubscription.Cancel(); err != nil {
		t.Fatalf("Cancel second: %v", err)
	}
	select {
	case <-watcher.closed:
	case <-time.After(time.Second):
		t.Fatal("idle monitor did not close the watcher after a failed Bind")
	}
}

func TestMonitorStopsAllSubscribersWhenNotificationsAreLost(t *testing.T) {
	watcher := newFakeInstanceWatcher()
	watcher.events = make(chan Event)
	watcher.lost = true
	monitor := newMonitor("PEAK USB", func() (instanceWatcher, error) {
		return watcher, nil
	})
	bus := newStoppedBus()
	subscription := subscribe(t, monitor, bus)
	defer subscription.Cancel()

	select {
	case err := <-bus.stopped:
		if !errors.Is(err, gocan.ErrHardwareDisconnected) {
			t.Fatalf("bus stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lost notification did not stop bus")
	}
}

func TestMonitorReleasesWatcherWhenIdle(t *testing.T) {
	var watchers []*fakeInstanceWatcher
	monitor := newMonitor("PEAK USB", func() (instanceWatcher, error) {
		watcher := newFakeInstanceWatcher()
		watchers = append(watchers, watcher)
		return watcher, nil
	})

	hold, err := monitor.Hold()
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := hold.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(watchers) != 1 {
		t.Fatalf("watchers started = %d, want 1", len(watchers))
	}
	select {
	case <-watchers[0].closed:
	case <-time.After(time.Second):
		t.Fatal("releasing the only hold did not close the watcher")
	}
	if err := hold.Release(); err != nil {
		t.Fatalf("repeated Release: %v", err)
	}

	bus := newStoppedBus()
	hold, err = monitor.Hold()
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	subscription, err := hold.Bind(func(err error) { bus.stopped <- err })
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if len(watchers) != 2 {
		t.Fatalf("watchers started = %d, want a fresh watcher after idle release", len(watchers))
	}
	// The pcan driver defers Release on every open path; after Bind it must be
	// a no-op that leaves the live subscription's watcher registered.
	if err := hold.Release(); err != nil {
		t.Fatalf("Release after Bind: %v", err)
	}
	select {
	case <-watchers[1].closed:
		t.Fatal("Release after Bind tore down the watcher under a live subscription")
	default:
	}
	if err := subscription.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-watchers[1].closed:
	case <-time.After(time.Second):
		t.Fatal("canceling the last subscription did not close the watcher")
	}
	if err := subscription.Cancel(); err != nil {
		t.Fatalf("repeated Cancel: %v", err)
	}
}

func TestMonitorIgnoresRetiredWatcher(t *testing.T) {
	var watchers []*fakeInstanceWatcher
	monitor := newMonitor("PEAK USB", func() (instanceWatcher, error) {
		watcher := newFakeInstanceWatcher()
		watchers = append(watchers, watcher)
		return watcher, nil
	})

	hold, err := monitor.Hold()
	if err != nil {
		t.Fatalf("first Hold: %v", err)
	}
	retiredStop := monitor.stop
	if err := hold.Release(); err != nil {
		t.Fatalf("release first Hold: %v", err)
	}

	bus := newStoppedBus()
	subscription := subscribe(t, monitor, bus)
	defer subscription.Cancel()
	monitor.stopAll(retiredStop, errors.New("retired watcher event"))
	select {
	case err := <-bus.stopped:
		t.Fatalf("retired watcher stopped a new subscriber: %v", err)
	default:
	}

	monitor.stopAll(monitor.stop, gocan.ErrHardwareDisconnected)
	select {
	case err := <-bus.stopped:
		if !errors.Is(err, gocan.ErrHardwareDisconnected) {
			t.Fatalf("active watcher stopped bus with %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active watcher did not stop its subscriber")
	}
}
