package cyclic_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/cyclic"
)

func TestGeneratedSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		anchor := time.Now()
		type transmission struct {
			at time.Duration
			id uint32
		}
		var sent []transmission
		bus := &callbackBus{send: func(_ context.Context, frame gocan.Frame) error {
			sent = append(sent, transmission{time.Since(anchor), frame.ID})
			time.Sleep(100 * time.Millisecond)
			return nil
		}}
		var calls uint32
		task, err := cyclic.StartFunc(context.Background(), bus, func() (gocan.Frame, error) {
			calls++
			time.Sleep(150 * time.Millisecond)
			return gocan.Frame{ID: calls}, nil
		}, 100*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		defer task.Stop()
		if elapsed := time.Since(anchor); elapsed != 250*time.Millisecond {
			t.Fatalf("StartFunc returned at %s, want 250ms", elapsed)
		}
		time.Sleep(600 * time.Millisecond)
		synctest.Wait()
		task.Stop()
		want := []transmission{{150 * time.Millisecond, 1}, {450 * time.Millisecond, 2}, {750 * time.Millisecond, 3}}
		if !slices.Equal(sent, want) || calls != 3 {
			t.Fatalf("sent = %v, calls = %d; want %v, 3", sent, calls, want)
		}
	})
}

func TestGeneratedFailures(t *testing.T) {
	generationErr := errors.New("generation failed")
	for _, test := range []struct {
		name                          string
		frame                         gocan.Frame
		generateErr, sendErr, wantErr error
	}{
		{"generation", gocan.Frame{}, generationErr, nil, generationErr},
		{"validation", gocan.Frame{ID: gocan.MaxStandardID + 1}, nil, nil, gocan.ErrInvalidFrame},
		{"send", gocan.Frame{}, nil, gocan.ErrTransmitQueueFull, gocan.ErrTransmitQueueFull},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls, sends := 0, 0
				generate := func() (gocan.Frame, error) {
					calls++
					if calls == 1 {
						return gocan.Frame{}, nil
					}
					return test.frame, test.generateErr
				}
				bus := &callbackBus{send: func(context.Context, gocan.Frame) error {
					sends++
					if calls == 1 {
						return nil
					}
					return test.sendErr
				}}
				task, err := cyclic.StartFunc(context.Background(), bus, generate, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				<-task.Done()
				task.Stop()
				wantSends := 1
				if test.sendErr != nil {
					wantSends = 2
				}
				if !errors.Is(task.Err(), test.wantErr) || calls != 2 || sends != wantSends {
					t.Fatalf("Err = %v, calls/sends = %d/%d; want %v, 2/%d", task.Err(), calls, sends, test.wantErr, wantSends)
				}
				// The same failure on the first callback must fail startup.
				if task, err := cyclic.StartFunc(context.Background(), bus, generate, time.Second); task != nil || !errors.Is(err, test.wantErr) {
					t.Fatalf("StartFunc = %v, %v; want nil, %v", task, err, test.wantErr)
				}
			})
		})
	}
}

func TestCallbackShutdown(t *testing.T) {
	for name, wantErr := range map[string]error{"stop": nil, "cancel": context.Canceled} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				started, release := make(chan struct{}), make(chan struct{})
				calls, sends := 0, 0
				bus := &callbackBus{send: func(context.Context, gocan.Frame) error {
					sends++
					return nil
				}}
				task, err := cyclic.StartFunc(ctx, bus, func() (gocan.Frame, error) {
					calls++
					if calls == 2 {
						close(started)
						<-release
					}
					return gocan.Frame{}, nil
				}, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				<-started
				stopped := make(chan struct{})
				go func() {
					if wantErr != nil {
						cancel()
						<-task.Done()
					} else {
						task.Stop()
					}
					close(stopped)
				}()
				synctest.Wait()
				select {
				case <-task.Done():
					t.Error("Done closed while callback was active")
				case <-stopped:
					t.Error("Stop returned while callback was active")
				default:
				}
				close(release)
				<-stopped
				task.Stop()
				if !errors.Is(task.Err(), wantErr) || calls != 2 || sends != 1 {
					t.Fatalf("Err = %v, calls/sends = %d/%d; want %v, 2/1", task.Err(), calls, sends, wantErr)
				}
			})
		})
	}
}

type callbackBus struct {
	send func(context.Context, gocan.Frame) error
}

func (bus *callbackBus) ID() gocan.BusID         { return 1 }
func (bus *callbackBus) Name() string            { return "callback" }
func (bus *callbackBus) Capture() *gocan.Capture { return nil }
func (bus *callbackBus) Send(ctx context.Context, frame gocan.Frame) error {
	return bus.send(ctx, frame)
}
func (bus *callbackBus) Done() <-chan struct{} { return nil }
func (bus *callbackBus) Err() error            { return nil }
func (bus *callbackBus) Close() error          { return nil }
