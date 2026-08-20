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

type memoryWriter struct {
	mu           sync.Mutex
	frames       []gocan.FrameEvent
	flushes      int
	flushStarted chan struct{}
	flushRelease chan struct{}
	flushErr     error
}

type failingWriter struct {
	remaining int
	err       error
	flushes   int
}

func (writer *failingWriter) WriteFrame(gocan.FrameEvent) error {
	if writer.remaining == 0 {
		return writer.err
	}
	writer.remaining--
	return nil
}

func (writer *failingWriter) WriteEvent(gocan.Event) error {
	return writer.err
}

func (writer *failingWriter) Flush() error {
	writer.flushes++
	return nil
}

func newMemoryWriter() *memoryWriter {
	return &memoryWriter{}
}

func (writer *memoryWriter) WriteFrame(event gocan.FrameEvent) error {
	writer.mu.Lock()
	writer.frames = append(writer.frames, event)
	writer.mu.Unlock()
	return nil
}

func (writer *memoryWriter) WriteEvent(gocan.Event) error { return nil }

func (writer *memoryWriter) Flush() error {
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

func (writer *memoryWriter) blockNextFlush(started, release chan struct{}) {
	writer.mu.Lock()
	writer.flushStarted = started
	writer.flushRelease = release
	writer.mu.Unlock()
}

func (writer *memoryWriter) failNextFlush(err error) {
	writer.mu.Lock()
	writer.flushErr = err
	writer.mu.Unlock()
}

func (writer *memoryWriter) flushCount() int {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.flushes
}

func (writer *memoryWriter) ids() []uint32 {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	ids := make([]uint32, len(writer.frames))
	for i, event := range writer.frames {
		ids[i] = event.Frame.ID
	}
	return ids
}

func awaitCursor(t *testing.T, task *recorder.Task, want gocan.Cursor) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); task.Cursor() != want; {
		if time.Now().After(deadline) {
			t.Fatalf("recorder cursor = %+v, want %+v", task.Cursor(), want)
		}
		runtime.Gosched()
	}
}

func appendFrame(t *testing.T, capture *gocan.Capture, id uint32) {
	t.Helper()
	frame, err := gocan.NewFrame(id, []byte{byte(id)}, 0)
	if err != nil {
		t.Fatalf("NewFrame %#x: %v", id, err)
	}
	err = capture.Append(gocan.FrameEvent{
		Bus:       1,
		Timestamp: time.Now(),
		Direction: gocan.DirectionReceive,
		Frame:     frame,
	})
	if err != nil {
		t.Fatalf("Append %#x: %v", id, err)
	}
}

func equalIDs(got []uint32, want ...uint32) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestRecordsRetainedAndLaterFramesInOrder(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x100)
	writer := newMemoryWriter()

	task, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(task.Stop)
	if got := writer.ids(); !equalIDs(got, 0x100) {
		t.Fatalf("frames written before Start returned = %#x, want 0x100", got)
	}
	if got := writer.flushCount(); got != 1 {
		t.Fatalf("flushes before Start returned = %d, want 1", got)
	}

	appendFrame(t, capture, 0x101)
	appendFrame(t, capture, 0x102)
	awaitCursor(t, task, capture.End())

	if got := writer.ids(); !equalIDs(got, 0x100, 0x101, 0x102) {
		t.Fatalf("recorded frame IDs = %#x, want 0x100 0x101 0x102", got)
	}
	if got, want := task.Cursor(), capture.End(); got != want {
		t.Fatalf("Cursor after catching up = %+v, want capture end %+v", got, want)
	}
}

func TestCursorWaitsForFlush(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x140)
	committed := capture.End()
	writer := newMemoryWriter()

	task, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Hour)
	if err != nil {
		t.Fatalf("Start: %v", err)
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
		task.Stop()
		close(stopped)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("final flush did not start")
	}
	if got := task.Cursor(); got != committed {
		t.Fatalf("Cursor during Flush = %+v, want prior committed cursor %+v", got, committed)
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
	if got, want := task.Cursor(), capture.End(); got != want {
		t.Fatalf("Cursor after Flush = %+v, want capture end %+v", got, want)
	}
}

func TestPartialWriteFlushesAcceptedProgress(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x180)
	accepted := capture.End()
	appendFrame(t, capture, 0x181)
	wantErr := errors.New("writer failed")

	writer := &failingWriter{remaining: 1, err: wantErr}
	task, err := recorder.Start(
		context.Background(),
		capture,
		writer,
		gocan.Cursor{},
		time.Second,
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Start error = %v, want %v", err, wantErr)
	}
	if task == nil {
		t.Fatal("Start returned no Task after partial progress")
	}
	if got := task.Cursor(); got != accepted {
		t.Fatalf("Cursor after initial failure = %+v, want %+v", got, accepted)
	}
	if got := writer.flushes; got != 1 {
		t.Fatalf("flushes after partial write = %d, want 1", got)
	}
	if got := task.Err(); !errors.Is(got, wantErr) {
		t.Fatalf("Task error = %v, want %v", got, wantErr)
	}
	select {
	case <-task.Done():
	default:
		t.Fatal("Task is still running after Start failed")
	}
}

func TestStopRecordsFinalWindow(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x200)
	writer := newMemoryWriter()

	// An interval no test run can reach leaves Stop as the only pass that can
	// record the frames appended below.
	task, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Hour)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	appendFrame(t, capture, 0x201)
	appendFrame(t, capture, 0x202)
	task.Stop()

	if got := writer.ids(); !equalIDs(got, 0x200, 0x201, 0x202) {
		t.Fatalf("recorded frame IDs = %#x, want 0x200 0x201 0x202", got)
	}
	if got := writer.flushCount(); got != 2 {
		t.Fatalf("flush count after Stop = %d, want initial and final flushes", got)
	}
	if err := task.Err(); err != nil {
		t.Fatalf("Err after Stop = %v, want nil", err)
	}
}

func TestFlushFailureKeepsCommittedCursor(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x280)
	committed := capture.End()
	writer := newMemoryWriter()

	task, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Hour)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	appendFrame(t, capture, 0x281)
	wantErr := errors.New("flush failed")
	writer.failNextFlush(wantErr)
	task.Stop()

	if got := task.Cursor(); got != committed {
		t.Fatalf("Cursor after failed Flush = %+v, want prior committed cursor %+v", got, committed)
	}
	if got := task.Err(); !errors.Is(got, wantErr) {
		t.Fatalf("Err after failed Flush = %v, want %v", got, wantErr)
	}
	if got := writer.flushCount(); got != 2 {
		t.Fatalf("flush count after failed final Flush = %d, want 2", got)
	}
}

func TestClearStopsWithCursorOutOfRange(t *testing.T) {
	capture := gocan.NewCapture()
	writer := newMemoryWriter()

	task, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(task.Stop)

	// The zero cursor survives any Clear, so the recorder must hold a real
	// position before the capture discards it.
	appendFrame(t, capture, 0x300)
	lost := capture.End()
	awaitCursor(t, task, lost)
	capture.Clear()

	select {
	case <-task.Done():
	case <-time.After(time.Second):
		t.Fatal("recorder did not stop after Clear")
	}
	if err := task.Err(); !errors.Is(err, gocan.ErrCursorOutOfRange) {
		t.Fatalf("Err after Clear = %v, want ErrCursorOutOfRange", err)
	}
	if got := task.Cursor(); got != lost {
		t.Fatalf("Cursor after Clear = %+v, want the last recorded position %+v", got, lost)
	}
}
