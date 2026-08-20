package recorder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tomrford/gocan"
)

// Writer receives capture records and delivers buffered output to its
// underlying io.Writer. Flush does not imply durable storage such as
// os.File.Sync.
type Writer interface {
	gocan.RecordWriter
	Flush() error
}

// Logger hands every record a Capture appends to one Writer.
//
// A Logger polls the capture every interval and writes with the same
// WriteRecordsSince pass as Task. Accepted reports how far the writer took
// those records. Committed reports the last accepted window whose Flush
// succeeded. The writer never sees a cursor: the Logger remembers the
// WriteRecordsSince result and publishes it as Committed only after Flush.
//
// If Flush fails, Accepted may have moved and Committed does not. A partial
// write that then flushes advances both through the last accepted record and
// the write error stops the Logger. Any writer error stops the Logger without
// a retry. One Logger owns one Writer exclusively until it stops. An
// asc.Writer can be passed directly to StartLogger.
//
// Callers prune to Committed, or to the oldest Committed cursor among several
// Loggers. Rotation, retention, and closing the writer belong to the caller.
// Stop writes and flushes one final window. Err reports the terminal error,
// or nil after Stop.
type Logger struct {
	capture  *gocan.Capture
	writer   Writer
	interval time.Duration

	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu        sync.Mutex
	accepted  gocan.Cursor
	committed gocan.Cursor
	err       error
	stopping  bool
}

// StartLogger writes every retained record appended after from, flushes them,
// then keeps logging until the Logger stops. Pass the zero gocan.Cursor to
// record everything the capture retains, or capture.End() to record only later
// records.
//
// StartLogger takes exclusive ownership of writer until the Logger stops. If
// the initial write or flush fails, StartLogger returns the stopped Logger and
// the error. Accepted and Committed show how far the writer got.
func StartLogger(
	ctx context.Context,
	capture *gocan.Capture,
	writer Writer,
	from gocan.Cursor,
	interval time.Duration,
) (*Logger, error) {
	if capture == nil {
		return nil, errors.New("recorder logger requires a capture")
	}
	if writer == nil {
		return nil, errors.New("recorder logger requires a writer")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("recorder logger interval must be positive: %s", interval)
	}

	taskContext, cancel := context.WithCancelCause(ctx)
	logger := &Logger{
		capture:   capture,
		writer:    writer,
		interval:  interval,
		ctx:       taskContext,
		cancel:    cancel,
		done:      make(chan struct{}),
		accepted:  from,
		committed: from,
	}
	if err := logger.pass(); err != nil {
		logger.finish(err)
		return logger, err
	}
	go logger.run()
	return logger, nil
}

// Accepted returns the position through which every record has been handed to
// the writer. It may be ahead of Committed while Flush is still running or
// after Flush failed. It is safe to call from any goroutine, including after
// the Logger stops, and never moves backwards.
func (logger *Logger) Accepted() gocan.Cursor {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return logger.accepted
}

// Committed returns the position through which every accepted record has been
// flushed through the writer. It is the cursor to prune to. It is safe to
// call from any goroutine, including after the Logger stops, and never moves
// backwards.
func (logger *Logger) Committed() gocan.Cursor {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return logger.committed
}

// Stop writes and flushes one final window, so records appended since the last
// pass are recorded rather than lost, then waits until logging has ended.
func (logger *Logger) Stop() {
	logger.mu.Lock()
	if !logger.stopping {
		logger.stopping = true
		logger.cancel(errStopRequested)
	}
	logger.mu.Unlock()
	<-logger.done
}

// Done is closed when logging stops.
func (logger *Logger) Done() <-chan struct{} {
	return logger.done
}

// Err returns the error that stopped the Logger. It returns nil before the
// Logger stops and when an explicit Stop ended it.
func (logger *Logger) Err() error {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return logger.err
}

func (logger *Logger) run() {
	var runErr error
	defer func() { logger.finish(runErr) }()

	ticker := time.NewTicker(logger.interval)
	defer ticker.Stop()

	for {
		select {
		case <-logger.ctx.Done():
			cause := context.Cause(logger.ctx)
			if err := logger.pass(); err != nil {
				runErr = err
				return
			}
			if !errors.Is(cause, errStopRequested) {
				runErr = cause
			}
			return

		case <-ticker.C:
			if err := logger.pass(); err != nil {
				runErr = err
				return
			}
		}
	}
}

// pass hands the capture's new records to the writer, publishes Accepted, then
// flushes before publishing Committed. An idle window or a failure that
// accepted nothing does not flush.
func (logger *Logger) pass() error {
	previous := logger.accepted
	next, writeErr := logger.capture.WriteRecordsSince(previous, logger.writer)

	logger.mu.Lock()
	logger.accepted = next
	logger.mu.Unlock()

	if next == previous {
		return writeErr
	}

	if err := logger.writer.Flush(); err != nil {
		return errors.Join(writeErr, fmt.Errorf("flush recorder writer: %w", err))
	}

	logger.mu.Lock()
	logger.committed = next
	logger.mu.Unlock()

	return writeErr
}

func (logger *Logger) finish(err error) {
	logger.cancel(err)
	logger.mu.Lock()
	logger.err = err
	logger.stopping = true
	close(logger.done)
	logger.mu.Unlock()
}
