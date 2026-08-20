package recorder_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/asc"
	"github.com/tomrford/gocan/recorder"
)

var _ recorder.Writer = (*asc.Writer)(nil)

type flushWriter struct {
	mu           sync.Mutex
	frames       []gocan.FrameEvent
	flushes      int
	flushStarted chan struct{}
	flushRelease chan struct{}
	flushErr     error
}

type failingFlushWriter struct {
	remaining int
	err       error
	flushes   int
}

func (writer *failingFlushWriter) WriteFrame(gocan.FrameEvent) error {
	if writer.remaining == 0 {
		return writer.err
	}
	writer.remaining--
	return nil
}

func (writer *failingFlushWriter) WriteEvent(gocan.Event) error {
	return writer.err
}

func (writer *failingFlushWriter) Flush() error {
	writer.flushes++
	return nil
}

func newFlushWriter() *flushWriter {
	return &flushWriter{}
}

func (writer *flushWriter) WriteFrame(event gocan.FrameEvent) error {
	writer.mu.Lock()
	writer.frames = append(writer.frames, event)
	writer.mu.Unlock()
	return nil
}

func (writer *flushWriter) WriteEvent(gocan.Event) error { return nil }

func (writer *flushWriter) Flush() error {
	writer.mu.Lock()
	writer.flushes++
	started := writer.flushStarted
	release := writer.flushRelease
	err := writer.flushErr
	writer.flushStarted = nil
	writer.flushRelease = nil
	writer.flushErr = nil
	writer.mu.Unlock()

	if started != nil {
		close(started)
	}
	if release != nil {
		<-release
	}
	return err
}

func (writer *flushWriter) blockNextFlush(started, release chan struct{}) {
	writer.mu.Lock()
	writer.flushStarted = started
	writer.flushRelease = release
	writer.mu.Unlock()
}

func (writer *flushWriter) failNextFlush(err error) {
	writer.mu.Lock()
	writer.flushErr = err
	writer.mu.Unlock()
}

func (writer *flushWriter) flushCount() int {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.flushes
}

func (writer *flushWriter) ids() []uint32 {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	ids := make([]uint32, len(writer.frames))
	for i, event := range writer.frames {
		ids[i] = event.Frame.ID
	}
	return ids
}

func awaitCommitted(t *testing.T, logger *recorder.Logger, want gocan.Cursor) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); logger.Committed() != want; {
		if time.Now().After(deadline) {
			t.Fatalf("logger committed = %+v, want %+v", logger.Committed(), want)
		}
		runtime.Gosched()
	}
}

func TestLoggerRecordsRetainedAndLaterFramesInOrder(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x100)
	writer := newFlushWriter()

	logger, err := recorder.StartLogger(context.Background(), capture, writer, gocan.Cursor{}, time.Millisecond)
	if err != nil {
		t.Fatalf("StartLogger: %v", err)
	}
	t.Cleanup(logger.Stop)
	if got := writer.ids(); !equalIDs(got, 0x100) {
		t.Fatalf("frames written before StartLogger returned = %#x, want 0x100", got)
	}
	if got := writer.flushCount(); got != 1 {
		t.Fatalf("flushes before StartLogger returned = %d, want 1", got)
	}
	if got, want := logger.Committed(), capture.End(); got != want {
		t.Fatalf("Committed after StartLogger = %+v, want %+v", got, want)
	}

	appendFrame(t, capture, 0x101)
	appendFrame(t, capture, 0x102)
	awaitCommitted(t, logger, capture.End())

	if got := writer.ids(); !equalIDs(got, 0x100, 0x101, 0x102) {
		t.Fatalf("recorded frame IDs = %#x, want 0x100 0x101 0x102", got)
	}
	if got, want := logger.Accepted(), capture.End(); got != want {
		t.Fatalf("Accepted after catching up = %+v, want %+v", got, want)
	}
}

func TestLoggerCommittedWaitsForFlush(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x140)
	committed := capture.End()
	writer := newFlushWriter()

	logger, err := recorder.StartLogger(context.Background(), capture, writer, gocan.Cursor{}, time.Hour)
	if err != nil {
		t.Fatalf("StartLogger: %v", err)
	}

	appendFrame(t, capture, 0x141)
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	writer.blockNextFlush(started, release)
	stopped := make(chan struct{})
	go func() {
		logger.Stop()
		close(stopped)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("final flush did not start")
	}
	if got := logger.Committed(); got != committed {
		t.Fatalf("Committed during Flush = %+v, want %+v", got, committed)
	}
	if got, want := logger.Accepted(), capture.End(); got != want {
		t.Fatalf("Accepted during Flush = %+v, want capture end %+v", got, want)
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned before Flush completed")
	default:
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after Flush completed")
	}
	if got, want := logger.Committed(), capture.End(); got != want {
		t.Fatalf("Committed after Flush = %+v, want capture end %+v", got, want)
	}
}

func TestLoggerPartialWriteCommitsAcceptedPrefix(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x180)
	accepted := capture.End()
	appendFrame(t, capture, 0x181)
	wantErr := errors.New("writer failed")

	writer := &failingFlushWriter{remaining: 1, err: wantErr}
	logger, err := recorder.StartLogger(
		context.Background(),
		capture,
		writer,
		gocan.Cursor{},
		time.Second,
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("StartLogger error = %v, want %v", err, wantErr)
	}
	if logger == nil {
		t.Fatal("StartLogger returned no Logger after partial progress")
	}
	if got := logger.Accepted(); got != accepted {
		t.Fatalf("Accepted after initial failure = %+v, want %+v", got, accepted)
	}
	if got := logger.Committed(); got != accepted {
		t.Fatalf("Committed after flushed partial write = %+v, want %+v", got, accepted)
	}
	if got := writer.flushes; got != 1 {
		t.Fatalf("flushes after partial write = %d, want 1", got)
	}
	if got := logger.Err(); !errors.Is(got, wantErr) {
		t.Fatalf("Logger error = %v, want %v", got, wantErr)
	}
	select {
	case <-logger.Done():
	default:
		t.Fatal("Logger is still running after StartLogger failed")
	}
}

func TestLoggerStopRecordsFinalWindow(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x200)
	writer := newFlushWriter()

	logger, err := recorder.StartLogger(context.Background(), capture, writer, gocan.Cursor{}, time.Hour)
	if err != nil {
		t.Fatalf("StartLogger: %v", err)
	}

	appendFrame(t, capture, 0x201)
	appendFrame(t, capture, 0x202)
	logger.Stop()

	if got := writer.ids(); !equalIDs(got, 0x200, 0x201, 0x202) {
		t.Fatalf("recorded frame IDs = %#x, want 0x200 0x201 0x202", got)
	}
	if got := writer.flushCount(); got != 2 {
		t.Fatalf("flush count after Stop = %d, want initial and final flushes", got)
	}
	if got, want := logger.Committed(), capture.End(); got != want {
		t.Fatalf("Committed after Stop = %+v, want %+v", got, want)
	}
	if err := logger.Err(); err != nil {
		t.Fatalf("Err after Stop = %v, want nil", err)
	}
}

func TestLoggerFlushFailureKeepsCommitted(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x280)
	committed := capture.End()
	writer := newFlushWriter()

	logger, err := recorder.StartLogger(context.Background(), capture, writer, gocan.Cursor{}, time.Hour)
	if err != nil {
		t.Fatalf("StartLogger: %v", err)
	}

	appendFrame(t, capture, 0x281)
	accepted := capture.End()
	wantErr := errors.New("flush failed")
	writer.failNextFlush(wantErr)
	logger.Stop()

	if got := logger.Accepted(); got != accepted {
		t.Fatalf("Accepted after failed Flush = %+v, want %+v", got, accepted)
	}
	if got := logger.Committed(); got != committed {
		t.Fatalf("Committed after failed Flush = %+v, want %+v", got, committed)
	}
	if got := logger.Err(); !errors.Is(got, wantErr) {
		t.Fatalf("Err after failed Flush = %v, want %v", got, wantErr)
	}
	if got := writer.flushCount(); got != 2 {
		t.Fatalf("flush count after failed final Flush = %d, want 2", got)
	}
}

func TestLoggerClearStopsWithCursorOutOfRange(t *testing.T) {
	capture := gocan.NewCapture()
	writer := newFlushWriter()

	logger, err := recorder.StartLogger(context.Background(), capture, writer, gocan.Cursor{}, time.Millisecond)
	if err != nil {
		t.Fatalf("StartLogger: %v", err)
	}
	t.Cleanup(logger.Stop)

	appendFrame(t, capture, 0x300)
	lost := capture.End()
	awaitCommitted(t, logger, lost)
	capture.Clear()

	select {
	case <-logger.Done():
	case <-time.After(time.Second):
		t.Fatal("logger did not stop after Clear")
	}
	if err := logger.Err(); !errors.Is(err, gocan.ErrCursorOutOfRange) {
		t.Fatalf("Err after Clear = %v, want ErrCursorOutOfRange", err)
	}
	if got := logger.Committed(); got != lost {
		t.Fatalf("Committed after Clear = %+v, want %+v", got, lost)
	}
	if got := logger.Accepted(); got != lost {
		t.Fatalf("Accepted after Clear = %+v, want %+v", got, lost)
	}
}
