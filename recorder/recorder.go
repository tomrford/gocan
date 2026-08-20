// Package recorder writes capture records to trace writers as they arrive.
package recorder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tomrford/gocan"
)

var errStopRequested = errors.New("recorder stop requested")

// Writer receives capture records and owns their format lifecycle. Flush
// delivers buffered records to the underlying writer. Close finalises the
// trace and flushes any remaining output; it does not necessarily close the
// underlying writer or make file data durable.
type Writer interface {
	gocan.RecordWriter
	Flush() error
	Close() error
}

// Recorder writes every record a Capture appends to one Writer.
//
// A Recorder polls the capture every interval, without blocking capture
// appends. Accepted reports records handed to the writer. Flushed reports the
// last accepted window whose Flush or Close succeeded and is the cursor to
// prune to. Flush does not imply durable storage such as os.File.Sync.
//
// Any writer error stops the Recorder without a retry. A cursor the capture
// can no longer place stops it with gocan.ErrCursorOutOfRange rather than
// hiding lost records. Stop is idempotent: it writes one final window, closes
// the writer, and waits for recording to end. Rotation and retention belong
// to the caller. Err reports the terminal error, or nil after a clean Stop.
type Recorder struct {
	capture  *gocan.Capture
	writer   Writer
	interval time.Duration

	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu       sync.Mutex
	accepted gocan.Cursor
	flushed  gocan.Cursor
	err      error
	stopping bool
}

// Start writes and flushes every retained record appended after from before
// it returns, then keeps recording until the Recorder stops. Pass the zero
// gocan.Cursor to record everything retained, or capture.End() to record only
// later records.
//
// Start takes ownership of writer's format lifecycle until it is closed. If
// the initial write, flush, or close fails, Start returns the stopped Recorder
// and its terminal error. Accepted and Flushed report how far it got.
func Start(
	ctx context.Context,
	capture *gocan.Capture,
	writer Writer,
	from gocan.Cursor,
	interval time.Duration,
) (*Recorder, error) {
	if capture == nil {
		return nil, errors.New("recorder requires a capture")
	}
	if writer == nil {
		return nil, errors.New("recorder requires a writer")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("recorder interval must be positive: %s", interval)
	}

	recorderContext, cancel := context.WithCancelCause(ctx)
	recorder := &Recorder{
		capture:  capture,
		writer:   writer,
		interval: interval,
		ctx:      recorderContext,
		cancel:   cancel,
		done:     make(chan struct{}),
		accepted: from,
		flushed:  from,
	}
	if err := recorder.pass(); err != nil {
		return recorder, recorder.finish(err)
	}
	go recorder.run()
	return recorder, nil
}

// Accepted returns the position through which every record has been handed to
// the writer. It may be ahead of Flushed while Flush is running or after a
// writer failure.
func (recorder *Recorder) Accepted() gocan.Cursor {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.accepted
}

// Flushed returns the position through which every accepted record has been
// flushed to the underlying writer. It is the cursor to prune to, but does not
// imply durable file storage.
func (recorder *Recorder) Flushed() gocan.Cursor {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.flushed
}

// Stop writes one final window, closes the writer, and waits for recording to
// end.
func (recorder *Recorder) Stop() {
	recorder.mu.Lock()
	if !recorder.stopping {
		recorder.stopping = true
		recorder.cancel(errStopRequested)
	}
	recorder.mu.Unlock()
	<-recorder.done
}

// Done is closed after the writer closes and recording stops.
func (recorder *Recorder) Done() <-chan struct{} {
	return recorder.done
}

// Err returns the error that stopped the Recorder. It returns nil before the
// Recorder stops and after a clean Stop.
func (recorder *Recorder) Err() error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.err
}

func (recorder *Recorder) run() {
	var runErr error
	defer func() { recorder.finish(runErr) }()

	ticker := time.NewTicker(recorder.interval)
	defer ticker.Stop()

	for {
		select {
		case <-recorder.ctx.Done():
			cause := context.Cause(recorder.ctx)
			if err := recorder.pass(); err != nil {
				runErr = err
				return
			}
			if !errors.Is(cause, errStopRequested) {
				runErr = cause
			}
			return

		case <-ticker.C:
			if err := recorder.pass(); err != nil {
				runErr = err
				return
			}
		}
	}
}

// pass publishes accepted progress, then flushes before publishing the safe
// prune cursor. An idle window or a failure that accepted nothing does not
// flush.
func (recorder *Recorder) pass() error {
	previous := recorder.accepted
	next, writeErr := recorder.capture.WriteRecordsSince(previous, recorder.writer)

	recorder.mu.Lock()
	recorder.accepted = next
	recorder.mu.Unlock()

	if next == previous {
		return writeErr
	}
	if err := recorder.writer.Flush(); err != nil {
		return errors.Join(writeErr, fmt.Errorf("flush recorder writer: %w", err))
	}

	recorder.mu.Lock()
	recorder.flushed = next
	recorder.mu.Unlock()
	return writeErr
}

// finish closes the format writer exactly once. A successful Close flushes
// every accepted record, including output left pending by a failed Flush.
func (recorder *Recorder) finish(runErr error) error {
	recorder.cancel(runErr)
	closeErr := recorder.writer.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close recorder writer: %w", closeErr)
	}

	recorder.mu.Lock()
	if closeErr == nil {
		recorder.flushed = recorder.accepted
	}
	recorder.err = errors.Join(runErr, closeErr)
	recorder.stopping = true
	close(recorder.done)
	err := recorder.err
	recorder.mu.Unlock()
	return err
}
