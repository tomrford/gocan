package recorder_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/asc"
	"github.com/tomrford/gocan/recorder"
)

var _ recorder.Writer = (*asc.Writer)(nil)

type writerProbe struct {
	mu sync.Mutex

	ids      []uint32
	closes   int
	failAt   int
	writeErr error
	flushErr error
	closeErr error

	flushStarted chan struct{}
	flushRelease chan struct{}
}

func (writer *writerProbe) WriteFrame(event gocan.FrameEvent) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.writeErr != nil && len(writer.ids) == writer.failAt {
		return writer.writeErr
	}
	writer.ids = append(writer.ids, event.Frame.ID)
	return nil
}

func (writer *writerProbe) WriteEvent(gocan.Event) error { return nil }

func (writer *writerProbe) Flush() error {
	writer.mu.Lock()
	started, release := writer.flushStarted, writer.flushRelease
	writer.flushStarted, writer.flushRelease = nil, nil
	err := writer.flushErr
	writer.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		<-release
	}
	return err
}

func (writer *writerProbe) Close() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.closes++
	return writer.closeErr
}

func (writer *writerProbe) blockNextFlush() (<-chan struct{}, chan struct{}) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	started := make(chan struct{})
	release := make(chan struct{})
	writer.flushStarted = started
	writer.flushRelease = release
	return started, release
}

func (writer *writerProbe) state() ([]uint32, int) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]uint32(nil), writer.ids...), writer.closes
}

func appendFrame(t *testing.T, capture *gocan.Capture, id uint32) {
	t.Helper()
	frame, err := gocan.NewFrame(id, []byte{byte(id)}, 0)
	if err != nil {
		t.Fatalf("NewFrame %#x: %v", id, err)
	}
	if err := capture.Append(gocan.FrameEvent{
		Bus:       1,
		Timestamp: time.Now(),
		Direction: gocan.DirectionReceive,
		Frame:     frame,
	}); err != nil {
		t.Fatalf("Append %#x: %v", id, err)
	}
}

func awaitFlushed(t *testing.T, recorder *recorder.Recorder, want gocan.Cursor) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for recorder.Flushed() != want {
		if time.Now().After(deadline) {
			t.Fatalf("Flushed = %+v, want %+v", recorder.Flushed(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRecorderLifecycle(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x100)
	writer := &writerProbe{}

	rec, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	appendFrame(t, capture, 0x101)
	awaitFlushed(t, rec, capture.End())
	appendFrame(t, capture, 0x102)
	rec.Stop()

	ids, closes := writer.state()
	if want := []uint32{0x100, 0x101, 0x102}; !slices.Equal(ids, want) {
		t.Fatalf("written IDs = %#x, want %#x", ids, want)
	}
	if closes != 1 {
		t.Fatalf("writer closes = %d, want 1", closes)
	}
	if rec.Accepted() != capture.End() || rec.Flushed() != capture.End() {
		t.Fatal("recorder did not finish at the capture end")
	}
	if err := rec.Err(); err != nil {
		t.Fatalf("Err after Stop = %v", err)
	}
}

func TestRecorderPublishesFlushBoundary(t *testing.T) {
	capture := gocan.NewCapture()
	appendFrame(t, capture, 0x140)
	writer := &writerProbe{}
	rec, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Hour)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	flushed := capture.End()
	appendFrame(t, capture, 0x141)
	started, release := writer.blockNextFlush()
	stopped := make(chan struct{})
	go func() {
		rec.Stop()
		close(stopped)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("final Flush did not start")
	}
	if rec.Accepted() != capture.End() || rec.Flushed() != flushed {
		t.Fatal("accepted and flushed cursors did not expose the blocked Flush")
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
}

func TestRecorderFailureCheckpoints(t *testing.T) {
	t.Run("partial write", func(t *testing.T) {
		capture := gocan.NewCapture()
		appendFrame(t, capture, 0x180)
		accepted := capture.End()
		appendFrame(t, capture, 0x181)
		writeErr := errors.New("write failed")
		writer := &writerProbe{failAt: 1, writeErr: writeErr}

		rec, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Second)
		if !errors.Is(err, writeErr) || rec == nil {
			t.Fatalf("Start = %#v, %v; want stopped Recorder and write error", rec, err)
		}
		if rec.Accepted() != accepted || rec.Flushed() != accepted {
			t.Fatal("successfully flushed prefix was not checkpointed")
		}
	})

	t.Run("flush and close", func(t *testing.T) {
		capture := gocan.NewCapture()
		appendFrame(t, capture, 0x200)
		flushErr := errors.New("flush failed")
		closeErr := errors.New("close failed")
		writer := &writerProbe{flushErr: flushErr, closeErr: closeErr}

		rec, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Second)
		if !errors.Is(err, flushErr) || !errors.Is(err, closeErr) {
			t.Fatalf("Start error = %v, want flush and close errors", err)
		}
		if rec.Accepted() != capture.End() || rec.Flushed() != (gocan.Cursor{}) {
			t.Fatal("failed lifecycle published an unsafe flush cursor")
		}
	})
}
