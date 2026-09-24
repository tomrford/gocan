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
		bus := &callbackBus{send: func(ctx context.Context, frame gocan.Frame) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			sent = append(sent, transmission{time.Since(anchor), frame.ID})
			time.Sleep(100 * time.Millisecond)
			return nil
		}}
		var calls uint32
		task, err := cyclic.StartFunc(context.Background(), bus, func() (gocan.Frame, error) {
			calls++
			time.Sleep(150 * time.Millisecond)
			return gocan.Frame{ID: calls}, nil
		}, cyclic.Config{Period: 100 * time.Millisecond, TransmitRetryTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			task.Stop()
			<-task.Done()
		}()
		if elapsed := time.Since(anchor); elapsed != 0 {
			t.Fatalf("StartFunc returned at %s, want 0", elapsed)
		}
		time.Sleep(850 * time.Millisecond)
		synctest.Wait()
		task.Stop()
		<-task.Done()
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
				task, err := cyclic.StartFunc(context.Background(), bus, generate, cyclic.Config{Period: time.Second})
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
				// First-attempt failures use the same completion path.
				task, err = cyclic.StartFunc(context.Background(), bus, generate, cyclic.Config{Period: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				<-task.Done()
				if !errors.Is(task.Err(), test.wantErr) {
					t.Fatalf("first attempt: Err = %v, want %v", task.Err(), test.wantErr)
				}
			})
		})
	}
}

func TestCallbackShutdown(t *testing.T) {
	for name, wantErr := range map[string]error{"stop": nil, "callback stop": nil, "cancel": context.Canceled, "cancel before start": context.Canceled} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if name == "cancel before start" {
					cancel()
				}
				started, release := make(chan struct{}), make(chan struct{})
				calls, sends := 0, 0
				bus := &callbackBus{send: func(context.Context, gocan.Frame) error {
					sends++
					return nil
				}}
				var task *cyclic.Task
				var err error
				task, err = cyclic.StartFunc(ctx, bus, func() (gocan.Frame, error) {
					calls++
					close(started)
					<-release
					if name == "callback stop" {
						task.Stop()
					}
					return gocan.Frame{ID: 1}, nil
				}, cyclic.Config{Period: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				if name == "cancel before start" {
					<-task.Done()
					if !errors.Is(task.Err(), wantErr) || calls != 0 || sends != 0 {
						t.Fatalf("Err = %v, calls/sends = %d/%d; want %v, 0/0", task.Err(), calls, sends, wantErr)
					}
					return
				}
				<-started
				if task.Frame() != (gocan.Frame{}) {
					t.Fatal("Frame is nonzero before the first callback returns")
				}
				if wantErr != nil {
					cancel()
				} else if name == "stop" {
					task.Stop()
				}
				select {
				case <-task.Done():
					t.Error("Done closed while callback was active")
				default:
				}
				close(release)
				<-task.Done()
				task.Stop()
				if !errors.Is(task.Err(), wantErr) || calls != 1 || sends != 0 || task.Frame().ID != 1 {
					t.Fatalf("Err = %v, calls/sends = %d/%d, Frame = %v; want %v, 1/0, ID 1", task.Err(), calls, sends, task.Frame(), wantErr)
				}
			})
		})
	}
}

func TestRetryScheduleAndStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		anchor := time.Now()
		var sent []uint32
		bus := &callbackBus{send: func(ctx context.Context, frame gocan.Frame) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			elapsed := time.Since(anchor)
			if elapsed >= 30*time.Millisecond || elapsed < 2*time.Millisecond || (elapsed >= 10*time.Millisecond && elapsed < 14*time.Millisecond) {
				return gocan.ErrTransmitQueueFull
			}
			sent = append(sent, frame.ID)
			return nil
		}}
		config := cyclic.Config{Period: 10 * time.Millisecond, TransmitRetryTimeout: 50 * time.Millisecond}
		task, err := cyclic.Start(context.Background(), bus, gocan.Frame{ID: 1}, config)
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(anchor); elapsed != 0 {
			t.Fatalf("Start returned at %s, want 0", elapsed)
		}
		time.Sleep(11 * time.Millisecond)
		synctest.Wait()
		// Update must not wait for the rejected occurrence to finish. That
		// occurrence retains its snapshot; the next observes the new frame.
		if err := task.Update(gocan.Frame{ID: 2}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if !slices.Equal(sent, []uint32{1, 1, 2}) {
			t.Fatalf("accepted frames: %v", sent)
		}
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		start := time.Now()
		task.Stop()
		<-task.Done()
		if task.Err() != nil || time.Now() != start {
			t.Fatalf("Stop: %v after %v", task.Err(), time.Since(start))
		}
		// A shorter retry budget and the next scheduled occurrence each bound
		// a generated send. Retries must not invoke the generator again.
		for _, budget := range []time.Duration{3 * time.Millisecond, 50 * time.Millisecond} {
			config.TransmitRetryTimeout = budget
			calls := 0
			start = time.Now()
			task, err := cyclic.StartFunc(context.Background(), bus, func() (gocan.Frame, error) {
				calls++
				return gocan.Frame{}, nil
			}, config)
			if err != nil {
				t.Fatal(err)
			}
			<-task.Done()
			if err := task.Err(); !errors.Is(err, gocan.ErrTransmitQueueFull) || errors.Is(err, context.DeadlineExceeded) || time.Since(start) != min(budget, config.Period) || calls != 1 {
				t.Fatalf("budget %v: %v after %v, generated %d times", budget, task.Err(), time.Since(start), calls)
			}
		}
	})
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
