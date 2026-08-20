package recorder_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/recorder"
)

type memoryWriter struct {
	mu     sync.Mutex
	frames []gocan.FrameEvent
}

type failingWriter struct {
	remaining int
	err       error
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

func TestStartReportsInitialFailureAndProgress(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x180)
	accepted := capture.End()
	appendFrame(t, capture, 0x181)
	wantErr := errors.New("writer failed")

	task, err := recorder.Start(
		context.Background(),
		capture,
		&failingWriter{remaining: 1, err: wantErr},
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
	if err := task.Err(); err != nil {
		t.Fatalf("Err after Stop = %v, want nil", err)
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
