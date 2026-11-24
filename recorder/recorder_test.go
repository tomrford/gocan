package recorder_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
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

func TestRecorderLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capture := gocan.NewCapture()
		appendFrame(t, capture, 0x100)
		writer := &writerProbe{}

		rec, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Millisecond)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = rec.Close() }()
		appendFrame(t, capture, 0x101)
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if rec.Flushed() != capture.End() {
			t.Fatalf("Flushed = %+v, want %+v", rec.Flushed(), capture.End())
		}
		appendFrame(t, capture, 0x102)
		if err := rec.Close(); err != nil {
			t.Fatalf("Close = %v", err)
		}
		if err := rec.Close(); err != nil {
			t.Fatalf("second Close = %v", err)
		}

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
			t.Fatalf("Err after Close = %v", err)
		}
	})
}

func TestRecorderPublishesFlushBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capture := gocan.NewCapture()
		appendFrame(t, capture, 0x140)
		writer := &writerProbe{}
		rec, err := recorder.Start(context.Background(), capture, writer, gocan.Cursor{}, time.Hour)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = rec.Close() }()
		flushed := capture.End()
		appendFrame(t, capture, 0x141)
		started, release := writer.blockNextFlush()
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		stopped := make(chan struct{})
		go func() {
			_ = rec.Close()
			close(stopped)
		}()
		defer func() {
			unblock()
			<-stopped
		}()

		synctest.Wait()
		select {
		case <-started:
		default:
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
		unblock()
		synctest.Wait()
		select {
		case <-stopped:
		default:
			t.Fatal("Stop did not return after Flush completed")
		}
	})
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

	t.Run("parent context cancel drains and joins cause with final flush error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			capture := gocan.NewCapture()
			writer := &writerProbe{}
			ctx, cancel := context.WithCancelCause(context.Background())
			rec, err := recorder.Start(ctx, capture, writer, gocan.Cursor{}, time.Hour)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}

			appendFrame(t, capture, 0x201)
			end := capture.End()
			cancel(gocan.ErrReceiveOverrun)
			<-rec.Done()

			if !errors.Is(rec.Err(), gocan.ErrReceiveOverrun) {
				t.Fatalf("Err = %v, want ErrReceiveOverrun", rec.Err())
			}
			if !errors.Is(rec.Close(), gocan.ErrReceiveOverrun) {
				t.Fatalf("Close = %v, want ErrReceiveOverrun", rec.Close())
			}
			if rec.Flushed() != end {
				t.Fatal("parent context cancel did not flush final window")
			}
		})

		synctest.Test(t, func(t *testing.T) {
			capture := gocan.NewCapture()
			flushErr := errors.New("storage full")
			writer := &writerProbe{}
			ctx, cancel := context.WithCancelCause(context.Background())
			rec, err := recorder.Start(ctx, capture, writer, gocan.Cursor{}, time.Hour)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}

			writer.mu.Lock()
			writer.flushErr = flushErr
			writer.mu.Unlock()
			appendFrame(t, capture, 0x202)
			cancel(gocan.ErrReceiveOverrun)
			<-rec.Done()

			if !errors.Is(rec.Err(), flushErr) || !errors.Is(rec.Err(), gocan.ErrReceiveOverrun) {
				t.Fatalf("Err = %v, want both flushErr and ErrReceiveOverrun", rec.Err())
			}
		})
	})
}
