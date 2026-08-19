// Package recorder writes capture records to a RecordWriter as they arrive.
package recorder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tomrford/gocan"
)

var errStopRequested = errors.New("recorder task stop requested")

// Task hands every record a Capture appends to one gocan.RecordWriter.
//
// A Task polls the capture every interval and advances by the cursor the
// capture returns, so it only reads and never blocks or slows an append. It
// makes no attempt to keep the writer's output bounded: rotation, retention,
// and pruning belong to the caller. Cursor reports delivery to the writer,
// not durable storage; prune to it only after a writer flush or commit succeeds.
//
// Any writer error stops the Task, without a retry, leaving Cursor at the last
// record the writer accepted. A cursor the capture can no longer place stops
// the Task with gocan.ErrCursorOutOfRange; a Task never resynchronises,
// because a lost position means records went unrecorded. Stop is idempotent
// and writes one final window before it returns. To record through shutdown,
// stop capture producers before Stop, then flush or close the writer after it.
// Err reports the terminal error, or nil after Stop.
type Task struct {
	capture  *gocan.Capture
	writer   gocan.RecordWriter
	interval time.Duration

	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu       sync.Mutex
	cursor   gocan.Cursor
	err      error
	stopping bool
}

// Start writes every retained record appended after from before it returns,
// then keeps recording until the Task stops. Pass the zero gocan.Cursor to
// record everything the capture retains, or capture.End() to record only later
// records.
//
// If the initial write fails, Start returns the stopped Task and the error;
// Cursor shows how far the writer got.
func Start(
	ctx context.Context,
	capture *gocan.Capture,
	writer gocan.RecordWriter,
	from gocan.Cursor,
	interval time.Duration,
) (*Task, error) {
	if capture == nil {
		return nil, errors.New("recorder task requires a capture")
	}
	if writer == nil {
		return nil, errors.New("recorder task requires a writer")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("recorder task interval must be positive: %s", interval)
	}

	taskContext, cancel := context.WithCancelCause(ctx)
	task := &Task{
		capture:  capture,
		writer:   writer,
		interval: interval,
		ctx:      taskContext,
		cancel:   cancel,
		done:     make(chan struct{}),
		cursor:   from,
	}
	if err := task.pass(); err != nil {
		task.finish(err)
		return task, err
	}
	go task.run()
	return task, nil
}

// Cursor returns the position through which every record has been handed to
// the writer. It is safe to call from any goroutine, including after the Task
// stops, and never moves backwards.
func (task *Task) Cursor() gocan.Cursor {
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.cursor
}

// Stop writes one final window, so records appended since the last pass are
// recorded rather than lost, then waits until recording has ended.
func (task *Task) Stop() {
	task.mu.Lock()
	if !task.stopping {
		task.stopping = true
		task.cancel(errStopRequested)
	}
	task.mu.Unlock()
	<-task.done
}

// Done is closed when recording stops.
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

func (task *Task) run() {
	var runErr error
	defer func() { task.finish(runErr) }()

	ticker := time.NewTicker(task.interval)
	defer ticker.Stop()

	for {
		select {
		case <-task.ctx.Done():
			cause := context.Cause(task.ctx)
			// One final pass: records appended since the pass above would
			// otherwise be lost from the end of the recording.
			if err := task.pass(); err != nil {
				runErr = err
				return
			}
			if !errors.Is(cause, errStopRequested) {
				runErr = cause
			}
			return

		case <-ticker.C:
			if err := task.pass(); err != nil {
				runErr = err
				return
			}
		}
	}
}

// pass hands the capture's new records to the writer. WriteRecordsSince
// reports the last record the writer accepted even when it fails, so a partial
// write and an unplaceable cursor both leave the position exactly where the
// writer stopped.
func (task *Task) pass() error {
	next, err := task.capture.WriteRecordsSince(task.cursor, task.writer)

	task.mu.Lock()
	task.cursor = next
	task.mu.Unlock()

	return err
}

func (task *Task) finish(err error) {
	task.cancel(err)
	task.mu.Lock()
	task.err = err
	task.stopping = true
	close(task.done)
	task.mu.Unlock()
}
