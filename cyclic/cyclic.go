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

// Task repeatedly sends one complete raw CAN frame.
//
// A Task sends immediately when started, then retains that schedule's original
// phase. If transmission falls behind, missed occurrences are skipped rather
// than sent as a burst. Update replaces the complete frame atomically; a send
// observes either the old frame or the new frame.
//
// Any send error stops the Task. Stop is idempotent and waits until no further
// sends can start. Err reports the terminal error, or nil after Stop.
//
type Task struct {
	bus    gocan.Bus
	period time.Duration
	frame  gocan.Frame

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
	if bus == nil {
		return nil, errors.New("cyclic task requires a bus")
	}
	if period <= 0 {
		return nil, fmt.Errorf("cyclic task period must be positive: %s", period)
	}
	if err := frame.Validate(); err != nil {
		return nil, err
	}

	anchor := time.Now()
	if err := bus.Send(ctx, frame); err != nil {
		return nil, err
	}

	taskContext, cancel := context.WithCancelCause(ctx)
	task := &Task{
		bus:    bus,
		period: period,
		frame:  frame,
		ctx:    taskContext,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go task.run(anchor)
	return task, nil
}

// Update atomically replaces the complete frame used by later sends.
func (task *Task) Update(frame gocan.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}

	task.mu.Lock()
	defer task.mu.Unlock()
	if err := task.operationErrorLocked(); err != nil {
		return err
	}
	task.frame = frame
	return nil
}

// Frame returns a snapshot of the complete frame used by later sends.
// The returned value can be passed to a semantic codec, then replaced with
// Update without retaining the frame originally passed to Start.
func (task *Task) Frame() gocan.Frame {
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.frame
}

// Stop stops recurring transmission and waits until no further sends can
// start. A native send already in progress is allowed to reach its definite
// result before Stop returns.
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
			cause := context.Cause(task.ctx)
			if !errors.Is(cause, errStopRequested) {
				runErr = cause
			}
			return

		case <-task.bus.Done():
			runErr = task.bus.Err()
			if runErr == nil {
				runErr = gocan.ErrBusClosed
			}
			return

		case <-timer.C:
			task.mu.Lock()
			if task.stopping {
				task.mu.Unlock()
				return
			}
			if task.ctx.Err() != nil {
				runErr = context.Cause(task.ctx)
				task.mu.Unlock()
				return
			}
			frame := task.frame
			err := task.bus.Send(task.ctx, frame)
			task.mu.Unlock()
			if err != nil {
				runErr = err
				return
			}

			next := nextDeadline(anchor, task.period, time.Now())
			timer.Reset(time.Until(next))
		}
	}
}

func (task *Task) finish(err error) {
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
