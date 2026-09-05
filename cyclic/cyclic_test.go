package cyclic_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/cyclic"
	"github.com/tomrford/gocan/drivers/virtual"
)

func TestTaskLifecycle(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	bus, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "cyclic"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	initial, err := gocan.NewFrame(0x123, []byte{1, 2, 3, 4}, 0)
	if err != nil {
		t.Fatalf("NewFrame initial: %v", err)
	}
	updated, err := gocan.NewFrame(0x123, []byte{5, 6, 7, 8}, 0)
	if err != nil {
		t.Fatalf("NewFrame updated: %v", err)
	}

	cursor := capture.End()
	task, err := cyclic.Start(context.Background(), bus, initial, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(task.Stop)

	key := gocan.FrameKey{
		Bus:       bus.ID(),
		ID:        initial.ID,
		Direction: gocan.DirectionTransmit,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	first, cursor, err := capture.Next(ctx, key, cursor)
	if err != nil {
		t.Fatalf("Next initial: %v", err)
	}
	if first.Frame != initial {
		t.Fatalf("initial cyclic frame = %+v, want %+v", first.Frame, initial)
	}

	if err := task.Update(updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := task.Frame(); got != updated {
		t.Fatalf("Frame after Update = %+v, want %+v", got, updated)
	}
	for {
		var next gocan.FrameEvent
		next, cursor, err = capture.Next(ctx, key, cursor)
		if err != nil {
			t.Fatalf("Next updated: %v", err)
		}
		if next.Frame == updated {
			break
		}
	}

	task.Stop()
	if err := task.Err(); err != nil {
		t.Fatalf("Err after Stop = %v, want nil", err)
	}
	if err := task.Update(initial); !errors.Is(err, cyclic.ErrStopped) {
		t.Fatalf("Update after Stop = %v, want ErrStopped", err)
	}

	before := len(capture.Series(key))
	time.Sleep(3 * 10 * time.Millisecond)
	if after := len(capture.Series(key)); after != before {
		t.Fatalf("capture gained %d transmissions after Stop", after-before)
	}
}

func TestStopWaitsForSendInProgress(t *testing.T) {
	bus := &blockingBus{
		sendStarted: make(chan struct{}, 1),
		releaseSend: make(chan struct{}),
	}
	t.Cleanup(func() {
		select {
		case <-bus.releaseSend:
		default:
			close(bus.releaseSend)
		}
	})
	frame, err := gocan.NewFrame(0x123, []byte{1}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	task, err := cyclic.Start(context.Background(), bus, frame, time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-bus.sendStarted:
	case <-time.After(time.Second):
		t.Fatal("recurring send did not start")
	}

	stopped := make(chan struct{})
	go func() {
		task.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while Send was still in progress")
	case <-time.After(10 * time.Millisecond):
	}

	close(bus.releaseSend)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after Send completed")
	}
	if err := task.Err(); err != nil {
		t.Fatalf("Err after Stop = %v, want nil", err)
	}
}

type blockingBus struct {
	sends       int
	sendStarted chan struct{}
	releaseSend chan struct{}
}

func (bus *blockingBus) ID() gocan.BusID { return 1 }

func (bus *blockingBus) Name() string { return "blocking" }

func (bus *blockingBus) Capture() *gocan.Capture { return nil }

func (bus *blockingBus) Send(context.Context, gocan.Frame) error {
	bus.sends++
	if bus.sends == 1 {
		return nil
	}
	bus.sendStarted <- struct{}{}
	<-bus.releaseSend
	return nil
}

func (bus *blockingBus) Done() <-chan struct{} { return nil }

func (bus *blockingBus) Err() error { return nil }

func (bus *blockingBus) Close() error { return nil }
