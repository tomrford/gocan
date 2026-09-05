// Package cyclic sends complete raw CAN frames on a recurring schedule.
package cyclic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tomrford/gocan"
)

// ErrStopped indicates an update to a task that has already stopped.
var ErrStopped = errors.New("cyclic task is stopped")

var errStopRequested = errors.New("cyclic task stop requested")

// Task repeatedly sends fixed or generated complete raw CAN frames.
//
// A Task sends immediately when started, then retains that schedule's original
// phase. If transmission falls behind, missed occurrences are skipped rather
// than sent as a burst. For fixed frames, Update replaces the complete frame
// atomically; a send observes either the old frame or the new frame.
//
// Any generation, validation, or send error stops the Task. Stop is idempotent
// and waits for any active callback or send to finish. Err reports the terminal
// error, or nil after Stop.
type Task struct {
	bus      gocan.Bus
	period   time.Duration
	frame    gocan.Frame
	generate func() (gocan.Frame, error)

	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu       sync.Mutex
	err      error
	stopping bool
}

// Start sends frame once, then starts recurring transmission every period.
// It returns only after the first send has been accepted by bus.
func Start(ctx context.Context, bus gocan.Bus, frame gocan.Frame, period time.Duration) (*Task, error) {
	return start(ctx, bus, frame, nil, period)
}

// StartFunc calls generate before each send, validates the returned frame, and
// sends it every period. It returns only after the first frame has been generated
// and accepted by bus. A failure before then returns a nil Task and the error.
//
// Calls to generate and Send are serial; missed periods do not call generate.
// The schedule starts before the first call to generate. Callbacks must return
// promptly, since cancellation and Stop cannot interrupt them. Callers must
// synchronise any state shared with the callback. A callback must not call Stop
// on its own Task, since Stop waits for the callback to return.
func StartFunc(ctx context.Context, bus gocan.Bus, generate func() (gocan.Frame, error), period time.Duration) (*Task, error) {
	if generate == nil {
		return nil, errors.New("cyclic task requires a frame callback")
	}
	return start(ctx, bus, gocan.Frame{}, generate, period)
}

func start(ctx context.Context, bus gocan.Bus, frame gocan.Frame, generate func() (gocan.Frame, error), period time.Duration) (*Task, error) {
	if bus == nil {
		return nil, errors.New("cyclic task requires a bus")
	}
	if period <= 0 {
		return nil, fmt.Errorf("cyclic task period must be positive: %s", period)
	}
	taskContext, cancel := context.WithCancelCause(ctx)
	task := &Task{
		bus:      bus,
		period:   period,
		frame:    frame,
		generate: generate,
		ctx:      taskContext,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	anchor := time.Now()
	if err := task.send(); err != nil {
		cancel(err)
		return nil, err
	}
	go task.run(anchor)
	return task, nil
}

// Update atomically replaces the complete frame used by later sends.
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
// frame, even if its send failed.
func (task *Task) Frame() gocan.Frame {
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.frame
}

// Stop stops recurring transmission and waits for any active callback or send
// to finish. A callback's result is not sent if Stop was requested while it ran.
// A native send already in progress is allowed to reach its definite result
// before Stop returns.
func (task *Task) Stop() {
	task.mu.Lock()
	if !task.stopping {
		task.stopping = true
		task.cancel(errStopRequested)
	}
	task.mu.Unlock()
	<-task.done
}

// Done is closed when recurring transmission stops.
func (task *Task) Done() <-chan struct{} {
	return task.done
}

// Err returns the error that stopped the Task. It returns nil before the Task
// stops and when an explicit Stop ended it.
func (task *Task) Err() error {
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.err
}

func (task *Task) run(anchor time.Time) {
	var runErr error
	defer func() { task.finish(runErr) }()

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
			if err := task.send(); err != nil {
				runErr = err
				return
			}

			next := nextDeadline(anchor, task.period, time.Now())
			timer.Reset(time.Until(next))
		}
	}
}

func (task *Task) send() error {
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
	defer task.mu.Unlock()
	if task.generate != nil {
		task.frame = generated
	} else if err := task.frame.Validate(); err != nil {
		return err
	}
	if err := context.Cause(task.ctx); err != nil {
		return err
	}
	return task.bus.Send(task.ctx, task.frame)
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
