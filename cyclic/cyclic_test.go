package cyclic_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
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
	task, err := cyclic.Start(context.Background(), bus, initial, cyclic.Config{Period: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		task.Stop()
		<-task.Done()
	})

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
	<-task.Done()
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

func TestStopDuringSend(t *testing.T) {
	for name, blockedSend := range map[string]int{"first": 1, "later": 2} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				sends := 0
				bus := &callbackBus{send: func(context.Context, gocan.Frame) error {
					sends++
					if sends == blockedSend {
						close(started)
						<-release
					}
					return nil
				}}
				task, err := cyclic.Start(context.Background(), bus, gocan.Frame{ID: 1}, cyclic.Config{Period: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				<-started
				task.Stop()
				task.Stop()
				if err := task.Update(gocan.Frame{ID: 2}); !errors.Is(err, cyclic.ErrStopped) {
					t.Fatalf("Update after Stop = %v, want ErrStopped", err)
				}
				select {
				case <-task.Done():
					t.Fatal("Done closed while Send was still in progress")
				default:
				}
				close(release)
				<-task.Done()
				if task.Err() != nil || sends != blockedSend {
					t.Fatalf("Err = %v, sends = %d; want nil, %d", task.Err(), sends, blockedSend)
				}
			})
		})
	}
}

func TestStartValidation(t *testing.T) {
	ctx := context.Background()
	bus := &callbackBus{send: func(context.Context, gocan.Frame) error {
		t.Error("invalid startup sent a frame")
		return nil
	}}
	config := cyclic.Config{Period: time.Second}
	for name, start := range map[string]func() (*cyclic.Task, error){
		"bus": func() (*cyclic.Task, error) {
			return cyclic.Start(ctx, nil, gocan.Frame{}, config)
		},
		"period": func() (*cyclic.Task, error) {
			return cyclic.Start(ctx, bus, gocan.Frame{}, cyclic.Config{})
		},
		"frame": func() (*cyclic.Task, error) {
			return cyclic.Start(ctx, bus, gocan.Frame{ID: gocan.MaxStandardID + 1}, config)
		},
		"callback": func() (*cyclic.Task, error) {
			return cyclic.StartFunc(ctx, bus, nil, config)
		},
		"retry timeout": func() (*cyclic.Task, error) {
			return cyclic.StartFunc(ctx, bus, func() (gocan.Frame, error) {
				t.Error("invalid startup called the generator")
				return gocan.Frame{}, nil
			}, cyclic.Config{Period: time.Second, TransmitRetryTimeout: -time.Second})
		},
	} {
		t.Run(name, func(t *testing.T) {
			task, err := start()
			if task != nil {
				task.Stop()
				<-task.Done()
			}
			if task != nil || err == nil {
				t.Fatalf("Start = %v, %v; want nil Task and argument error", task, err)
			}
		})
	}
}
