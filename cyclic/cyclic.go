// Package cyclic sends complete raw CAN frames on a recurring schedule.
package cyclic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tomrford/gocan"
)

// ErrStopped indicates an update to a task that has already stopped.
var ErrStopped = errors.New("cyclic task is stopped")

// ErrOccurrenceMissed indicates that an occurrence's send budget expired.
// It does not imply that the native transmit queue was full.
var ErrOccurrenceMissed = errors.New("cyclic occurrence send budget expired")

var errStopRequested = errors.New("cyclic task stop requested")

// Config controls a recurring transmission schedule.
type Config struct {
	// Period is the positive interval between scheduled occurrences.
	Period time.Duration
	// TransmitRetryTimeout bounds queue-full retries for each occurrence.
	// Zero disables retries and the occurrence deadline. A positive value also
	// bounds sending by the next scheduled occurrence after frame generation.
	// Native sends in progress finish with their definite result even if that
	// deadline elapses.
	TransmitRetryTimeout time.Duration
	// MaxConsecutiveMisses permits this many consecutive send failures matching
	// ErrOccurrenceMissed or gocan.ErrTransmitQueueFull before the next stops the
	// task. Zero stops on the first failure. Acceptance resets the count.
	// Cancellation, bus closure, and generation or other send errors always stop.
	MaxConsecutiveMisses uint
}

// Task repeatedly sends fixed or generated complete raw CAN frames.
//
// A Task attempts its first send asynchronously without an initial delay, then
// retains the schedule's original phase. If transmission falls behind, missed
// occurrences are skipped rather than sent as a burst. For fixed frames, Update
// replaces the complete frame atomically; a send observes either the old frame
// or the new frame.
//
// Generation, validation, and send errors stop the Task, including on the first
// attempt, except for send misses permitted by Config.MaxConsecutiveMisses.
// Stop requests cancellation; Done closes after all callbacks and sends finish.
// Err then reports the terminal error, or nil if Stop ended it.
type Task struct {
	bus          gocan.Bus
	period       time.Duration
	retryTimeout time.Duration
	maxMisses    uint
	misses       uint          // Consecutive misses, owned by the send loop.
	missed       atomic.Uint64 // Total misses, tolerated or terminal.
	frame        gocan.Frame
	generate     func() (gocan.Frame, error)

	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu       sync.Mutex
	err      error
	stopping bool
}

// Start starts recurring transmission of frame and returns without waiting for
// the first send. It returns an error only for invalid arguments; transmission
// failures are reported through Task.Done and Task.Err.
func Start(ctx context.Context, bus gocan.Bus, frame gocan.Frame, config Config) (*Task, error) {
	return start(ctx, bus, frame, nil, config)
}

// StartFunc calls generate before each send, validates the returned frame, and
// sends it every period. It returns without waiting for the first callback or
// send. It returns an error only for invalid arguments; generation, validation,
// and transmission failures are reported through Task.Done and Task.Err.
//
// Calls to generate and Send are serial; missed periods do not call generate.
// The schedule starts before the first call to generate. Callbacks must return
// promptly, since cancellation and Stop cannot interrupt them. Callers must
// synchronise any state shared with the callback, which may run before StartFunc
// returns. A callback may call Stop, but must not wait on its own Task's Done.
// If the callback uses the returned Task, synchronise its publication (for
// example, through a channel) before accessing it.
func StartFunc(ctx context.Context, bus gocan.Bus, generate func() (gocan.Frame, error), config Config) (*Task, error) {
	if generate == nil {
		return nil, errors.New("cyclic task requires a frame callback")
	}
	return start(ctx, bus, gocan.Frame{}, generate, config)
}

func start(ctx context.Context, bus gocan.Bus, frame gocan.Frame, generate func() (gocan.Frame, error), config Config) (*Task, error) {
	if bus == nil {
		return nil, errors.New("cyclic task requires a bus")
	}
	if config.Period <= 0 {
		return nil, fmt.Errorf("cyclic task period must be positive: %s", config.Period)
	}
	if config.TransmitRetryTimeout < 0 {
		return nil, errors.New("CAN transmit retry timeout must not be negative")
	}
	if generate == nil {
		if err := frame.Validate(); err != nil {
			return nil, err
		}
	}
	taskContext, cancel := context.WithCancelCause(ctx)
	task := &Task{
		bus:          bus,
		period:       config.Period,
		retryTimeout: config.TransmitRetryTimeout,
		maxMisses:    config.MaxConsecutiveMisses,
		frame:        frame,
		generate:     generate,
		ctx:          taskContext,
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	go task.run(time.Now())
	return task, nil
}

// Update atomically replaces the complete frame used by later sends.
// It may replace the initial frame before the first send snapshots it.
// It returns an error for a Task created with StartFunc.
func (task *Task) Update(frame gocan.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}

	task.mu.Lock()
	defer task.mu.Unlock()
	if err := task.operationErrorLocked(); err != nil {
		return err
	}
	if task.generate != nil {
		return errors.New("cannot update a cyclic task with a frame callback")
	}
	task.frame = frame
	return nil
}

// Frame returns a snapshot of the complete frame used by later sends.
// The returned value can be passed to a semantic codec, then replaced with
// Update without retaining the frame originally passed to Start.
// For a Task created with StartFunc, Frame returns the last valid generated
// frame, even if its send failed, or the zero frame before one is generated.
func (task *Task) Frame() gocan.Frame {
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.frame
}

// Stop requests cancellation and returns without waiting for an active callback
// or send. It is idempotent. To wait for completion, receive from Done after Stop.
// A callback's result is not sent if Stop was requested while it ran. A native
// send already in progress finishes with its definite result before Done closes.
func (task *Task) Stop() {
	task.mu.Lock()
	if !task.stopping {
		task.stopping = true
		task.cancel(errStopRequested)
	}
	task.mu.Unlock()
}

// Done is closed when the Task has finished all callbacks and sends.
func (task *Task) Done() <-chan struct{} {
	return task.done
}

// Err returns the error that stopped the Task. It returns nil before the Task
// stops and when an explicit Stop ended it. A nil error before Done closes does
// not imply that any send has succeeded.
func (task *Task) Err() error {
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.err
}

// Missed counts occurrences whose send failed with ErrOccurrenceMissed or
// gocan.ErrTransmitQueueFull, including a failure that stopped the Task.
// Callers polling it can report misses that Config.MaxConsecutiveMisses
// absorbed, which Err never surfaces.
func (task *Task) Missed() uint64 {
	return task.missed.Load()
}

func (task *Task) run(anchor time.Time) {
	var runErr error
	defer func() { task.finish(runErr) }()

	if runErr = task.send(anchor); runErr != nil {
		return
	}

	timer := time.NewTimer(time.Until(nextDeadline(anchor, task.period, time.Now())))
	defer timer.Stop()

	for {
		select {
		case <-task.ctx.Done():
			runErr = context.Cause(task.ctx)
			return

		case <-task.bus.Done():
			runErr = task.bus.Err()
			if runErr == nil {
				runErr = gocan.ErrBusClosed
			}
			return

		case <-timer.C:
			if err := task.send(anchor); err != nil {
				runErr = err
				return
			}

			next := nextDeadline(anchor, task.period, time.Now())
			timer.Reset(time.Until(next))
		}
	}
}

func (task *Task) send(anchor time.Time) error {
	if err := context.Cause(task.ctx); err != nil {
		return err
	}

	// Run user code without the state lock so Stop can request cancellation and
	// the callback can inspect the Task. The single send loop prevents overlap.
	var generated gocan.Frame
	if task.generate != nil {
		var err error
		generated, err = task.generate()
		if err != nil {
			return err
		}
		if err := generated.Validate(); err != nil {
			return err
		}
	}

	task.mu.Lock()
	if task.generate != nil {
		task.frame = generated
	}
	frame := task.frame
	task.mu.Unlock()
	if err := context.Cause(task.ctx); err != nil {
		return err
	}
	ctx := task.ctx
	if task.retryTimeout > 0 {
		var cancel context.CancelFunc
		// Occurrence expiry alone does not prove queue saturation. If a retry
		// was rejected, Send also retains that rejection in its returned error.
		ctx, cancel = context.WithDeadlineCause(ctx, nextDeadline(anchor, task.period, time.Now()), ErrOccurrenceMissed)
		defer cancel()
	}
	err := gocan.Send(ctx, task.bus, frame, task.retryTimeout)
	// A driver can observe occurrence expiry before Stop or caller cancellation.
	// Resolve context errors against the task's cause after the send finishes.
	if task.ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return context.Cause(task.ctx)
	}
	if err == nil {
		task.misses = 0
	} else if task.ctx.Err() == nil && (errors.Is(err, ErrOccurrenceMissed) || errors.Is(err, gocan.ErrTransmitQueueFull)) {
		select {
		case <-task.bus.Done():
			return err
		default:
		}
		task.missed.Add(1)
		if task.misses < task.maxMisses {
			task.misses++
			return nil
		}
	}
	return err
}

func (task *Task) finish(err error) {
	if errors.Is(err, errStopRequested) {
		err = nil
	}
	task.cancel(err)
	task.mu.Lock()
	task.err = err
	task.stopping = true
	close(task.done)
	task.mu.Unlock()
}

func (task *Task) operationErrorLocked() error {
	if task.err != nil {
		return task.err
	}
	if task.stopping {
		return ErrStopped
	}
	if task.ctx.Err() != nil {
		return context.Cause(task.ctx)
	}
	return nil
}

func nextDeadline(anchor time.Time, period time.Duration, now time.Time) time.Time {
	elapsed := now.Sub(anchor)
	if elapsed < 0 {
		return anchor
	}
	return anchor.Add((elapsed/period + 1) * period)
}
