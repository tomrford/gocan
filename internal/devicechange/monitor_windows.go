//go:build windows

package devicechange

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tomrford/gocan"
)

const lostCheckInterval = 100 * time.Millisecond

// instanceWatcher is the notification source a Monitor consumes. Production
// monitors use *Watcher; tests inject fakes.
type instanceWatcher interface {
	Events() <-chan Event
	Lost() bool
	Close() error
}

// Monitor turns Windows device-instance removals for one vendor's instance-ID
// prefix into stop calls on every subscribed bus. Its watcher filters to
// matching removals before the finite notification queue, so a removal or a
// lost notification is one shared failure domain: every subscriber stops with
// gocan.ErrHardwareDisconnected.
//
// The Windows registration and its polling goroutine exist only while at
// least one Hold or Subscription is outstanding, so failed or closed opens do
// not accumulate registrations. The one process-wide callback trampoline is
// permanent by Go runtime design and shared by every registration.
type Monitor struct {
	description string
	watch       func() (instanceWatcher, error)

	mu          sync.Mutex
	watcher     instanceWatcher
	stop        chan struct{}
	done        chan struct{}
	generation  uint64
	holds       int
	subscribers map[*Subscription]struct{}
}

// NewMonitor watches for removals of device instances whose IDs start with
// prefix, compared case-insensitively. description names the hardware family
// in errors, for example "PEAK USB".
func NewMonitor(prefix, description string) *Monitor {
	return newMonitor(description, func() (instanceWatcher, error) {
		return WatchRemovals(prefix)
	})
}

func newMonitor(description string, watch func() (instanceWatcher, error)) *Monitor {
	return &Monitor{
		description: description,
		watch:       watch,
		subscribers: make(map[*Subscription]struct{}),
	}
}

// Hold is an open in progress: it pins the device generation observed before
// the native channel was initialized. Every Hold must be ended by exactly one
// Bind or Release.
type Hold struct {
	monitor    *Monitor
	generation uint64
	ended      bool
}

// Subscription is one bus registered for stop fan-out.
type Subscription struct {
	monitor  *Monitor
	stopBus  func(error)
	canceled bool
}

// Hold starts (or joins) the Windows notification watcher and pins the current
// device generation.
func (monitor *Monitor) Hold() (*Hold, error) {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if monitor.watcher == nil {
		watcher, err := monitor.watch()
		if err != nil {
			return nil, err
		}
		monitor.watcher = watcher
		monitor.stop = make(chan struct{})
		monitor.done = make(chan struct{})
		go monitor.run(watcher, monitor.stop, monitor.done)
	}
	monitor.holds++
	return &Hold{monitor: monitor, generation: monitor.generation}, nil
}

// Bind converts the hold into a stop subscription for one opened bus. It fails
// if a matching device was removed since the hold was taken, so an open cannot
// complete against hardware that disappeared mid-open.
func (hold *Hold) Bind(stopBus func(error)) (*Subscription, error) {
	monitor := hold.monitor
	monitor.mu.Lock()
	if hold.ended {
		monitor.mu.Unlock()
		return nil, errors.New("device hold was already ended")
	}
	hold.ended = true
	monitor.holds--
	if hold.generation != monitor.generation {
		shutdown := monitor.idleShutdownLocked()
		monitor.mu.Unlock()
		err := fmt.Errorf(
			"%w: a %s device changed while the channel was opening",
			gocan.ErrHardwareDisconnected,
			monitor.description,
		)
		if shutdown != nil {
			err = errors.Join(err, shutdown())
		}
		return nil, err
	}
	subscription := &Subscription{monitor: monitor, stopBus: stopBus}
	monitor.subscribers[subscription] = struct{}{}
	monitor.mu.Unlock()
	return subscription, nil
}

// Release ends the hold without subscribing. It is a no-op after Bind, so a
// deferred Release cleans up every failed-open path.
func (hold *Hold) Release() error {
	monitor := hold.monitor
	monitor.mu.Lock()
	if hold.ended {
		monitor.mu.Unlock()
		return nil
	}
	hold.ended = true
	monitor.holds--
	shutdown := monitor.idleShutdownLocked()
	monitor.mu.Unlock()
	if shutdown != nil {
		return shutdown()
	}
	return nil
}

// Cancel removes the subscription. It is safe to call more than once.
func (subscription *Subscription) Cancel() error {
	monitor := subscription.monitor
	monitor.mu.Lock()
	if subscription.canceled {
		monitor.mu.Unlock()
		return nil
	}
	subscription.canceled = true
	delete(monitor.subscribers, subscription)
	shutdown := monitor.idleShutdownLocked()
	monitor.mu.Unlock()
	if shutdown != nil {
		return shutdown()
	}
	return nil
}

// idleShutdownLocked detaches the watcher state when nothing references it and
// returns the blocking teardown to run outside the monitor lock. run calls
// stopAll, which takes the lock, so waiting for done under it would deadlock.
func (monitor *Monitor) idleShutdownLocked() func() error {
	if monitor.holds > 0 || len(monitor.subscribers) > 0 || monitor.watcher == nil {
		return nil
	}
	watcher, stop, done := monitor.watcher, monitor.stop, monitor.done
	monitor.watcher, monitor.stop, monitor.done = nil, nil, nil
	return func() error {
		close(stop)
		<-done
		return watcher.Close()
	}
}

func (monitor *Monitor) run(watcher instanceWatcher, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(lostCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case event := <-watcher.Events():
			monitor.stopAll(stop, fmt.Errorf(
				"%w: Windows removed %s device %q",
				gocan.ErrHardwareDisconnected,
				monitor.description,
				event.InstanceID,
			))
		case <-ticker.C:
			// TODO: Requalify physical removal on both PEAK stacks: the
			// existing qualification predates the shared-callback dispatch,
			// and this fail-closed stop on a lost notification has never been
			// observed on hardware.
			if watcher.Lost() {
				monitor.stopAll(stop, fmt.Errorf(
					"%w: Windows device notifications were lost",
					gocan.ErrHardwareDisconnected,
				))
			}
		case <-stop:
			return
		}
	}
}

func (monitor *Monitor) stopAll(source <-chan struct{}, err error) {
	monitor.mu.Lock()
	if monitor.stop != source {
		monitor.mu.Unlock()
		return
	}
	monitor.generation++
	subscribers := make([]*Subscription, 0, len(monitor.subscribers))
	for subscription := range monitor.subscribers {
		subscribers = append(subscribers, subscription)
	}
	monitor.mu.Unlock()
	for _, subscription := range subscribers {
		subscription.stopBus(err)
	}
}
