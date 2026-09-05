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

func TestStartFuncRejectsInvalidArguments(t *testing.T) {
	bus := &callbackBus{send: func(context.Context, gocan.Frame) error {
		t.Fatal("sent a frame with invalid arguments")
		return nil
	}}
	generate := func() (gocan.Frame, error) {
		t.Fatal("called generate with invalid arguments")
		return gocan.Frame{}, nil
	}
	for _, test := range []struct {
		name     string
		bus      gocan.Bus
		generate func() (gocan.Frame, error)
		period   time.Duration
	}{
		{"nil bus", nil, generate, time.Second},
		{"nil callback", bus, nil, time.Second},
		{"zero period", bus, generate, 0},
		{"negative period", bus, generate, -time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			task, err := cyclic.StartFunc(context.Background(), test.bus, test.generate, test.period)
			if task != nil || err == nil {
				t.Fatalf("StartFunc = %v, %v, want nil Task and an error", task, err)
			}
		})
	}
}

func TestScheduleSkipsMissedPeriods(t *testing.T) {
	for _, generated := range []bool{false, true} {
		name := "fixed"
		if generated {
			name = "generated"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				anchor := time.Now()
				var calls, sends []time.Duration
				var frames []gocan.Frame
				generate := func() (gocan.Frame, error) {
					calls = append(calls, time.Since(anchor))
					time.Sleep(150 * time.Millisecond)
					return gocan.Frame{ID: uint32(len(calls))}, nil
				}
				bus := &callbackBus{send: func(_ context.Context, frame gocan.Frame) error {
					sends = append(sends, time.Since(anchor))
					frames = append(frames, frame)
					delay := 250 * time.Millisecond
					if generated {
						delay = 100 * time.Millisecond
					}
					time.Sleep(delay)
					return nil
				}}
				var task *cyclic.Task
				var err error
				if generated {
					task, err = cyclic.StartFunc(context.Background(), bus, generate, 100*time.Millisecond)
				} else {
					task, err = cyclic.Start(context.Background(), bus, gocan.Frame{ID: 1}, 100*time.Millisecond)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer task.Stop()
				if elapsed := time.Since(anchor); elapsed != 250*time.Millisecond {
					t.Fatalf("Start returned at %s, want 250ms", elapsed)
				}
				if generated && task.Update(gocan.Frame{ID: 99}) == nil {
					t.Fatal("Update accepted a frame for a callback task")
				}
				time.Sleep(600 * time.Millisecond)
				synctest.Wait()
				task.Stop()
				want := []time.Duration{0, 300 * time.Millisecond, 600 * time.Millisecond}
				if generated {
					if !slices.Equal(calls, want) {
						t.Fatalf("callback times = %v, want %v", calls, want)
					}
					for i := range want {
						want[i] += 150 * time.Millisecond
					}
				}
				if !slices.Equal(sends, want) {
					t.Fatalf("send times = %v, want %v", sends, want)
				}
				for i, frame := range frames {
					id := uint32(1)
					if generated {
						id = uint32(i + 1)
					}
					if frame.ID != id {
						t.Fatalf("frame %d ID = %d, want %d", i, frame.ID, id)
					}
				}
				if frame := task.Frame(); frame != frames[len(frames)-1] {
					t.Fatalf("Frame = %+v, want last generated frame", frame)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if len(sends) != 3 || (generated && len(calls) != 3) {
					t.Fatalf("work continued after Stop: %d calls, %d sends", len(calls), len(sends))
				}
			})
		})
	}
}

func TestStartFuncFailures(t *testing.T) {
	generationErr := errors.New("generation failed")
	for _, stage := range []string{"initial", "recurring"} {
		for _, failure := range []struct {
			name string
			err  error
		}{
			{"generation", generationErr},
			{"validation", gocan.ErrInvalidFrame},
			{"send", gocan.ErrTransmitQueueFull},
		} {
			t.Run(stage+"/"+failure.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					failAt := 1
					if stage == "recurring" {
						failAt = 2
					}
					calls, sends := 0, 0
					bus := &callbackBus{send: func(context.Context, gocan.Frame) error {
						sends++
						if calls == failAt && failure.name == "send" {
							return failure.err
						}
						return nil
					}}
					task, err := cyclic.StartFunc(context.Background(), bus, func() (gocan.Frame, error) {
						calls++
						if calls == failAt {
							switch failure.name {
							case "generation":
								return gocan.Frame{}, failure.err
							case "validation":
								return gocan.Frame{ID: gocan.MaxStandardID + 1}, nil
							}
						}
						return gocan.Frame{ID: uint32(calls)}, nil
					}, time.Second)
					if stage == "initial" {
						if task != nil || !errors.Is(err, failure.err) {
							t.Fatalf("StartFunc = %v, %v, want nil, %v", task, err, failure.err)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						<-task.Done()
						task.Stop()
						if !errors.Is(task.Err(), failure.err) {
							t.Fatalf("Err = %v, want %v", task.Err(), failure.err)
						}
						wantID := uint32(1)
						if failure.name == "send" {
							wantID = 2
						}
						if frame := task.Frame(); frame.ID != wantID {
							t.Fatalf("Frame ID = %d, want %d", frame.ID, wantID)
						}
					}
					time.Sleep(3 * time.Second)
					synctest.Wait()
					wantSends := failAt - 1
					if failure.name == "send" {
						wantSends++
					}
					if calls != failAt || sends != wantSends {
						t.Fatalf("calls/sends = %d/%d, want %d/%d", calls, sends, failAt, wantSends)
					}
				})
			})
		}
	}
}

func TestStartFuncCancellation(t *testing.T) {
	for _, stage := range []string{"before start", "initial callback", "recurring callback", "between sends"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				cause := errors.New("caller cancelled")
				calls, sends := 0, 0
				bus := &callbackBus{send: func(context.Context, gocan.Frame) error {
					sends++
					return nil
				}}
				if stage == "before start" {
					cancel(cause)
				}
				task, err := cyclic.StartFunc(ctx, bus, func() (gocan.Frame, error) {
					calls++
					if stage == "initial callback" || (stage == "recurring callback" && calls == 2) {
						cancel(cause)
					}
					return gocan.Frame{}, nil
				}, time.Second)
				wantCalls, wantSends := 0, 0
				switch stage {
				case "before start", "initial callback":
					if stage == "initial callback" {
						wantCalls = 1
					}
					if task != nil || !errors.Is(err, cause) {
						t.Fatalf("StartFunc = %v, %v, want nil, %v", task, err, cause)
					}
				default:
					if err != nil {
						t.Fatal(err)
					}
					wantCalls, wantSends = 2, 1
					if stage == "between sends" {
						wantCalls = 1
						cancel(cause)
					}
					<-task.Done()
					task.Stop()
					if !errors.Is(task.Err(), cause) {
						t.Fatalf("Err = %v, want %v", task.Err(), cause)
					}
				}
				time.Sleep(3 * time.Second)
				synctest.Wait()
				if calls != wantCalls || sends != wantSends {
					t.Fatalf("calls/sends = %d/%d, want %d/%d", calls, sends, wantCalls, wantSends)
				}
			})
		})
	}
}

func TestStartFuncCancellationDuringSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		calls, sends := 0, 0
		bus := &callbackBus{send: func(ctx context.Context, _ gocan.Frame) error {
			sends++
			if sends == 1 {
				return nil
			}
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}}
		task, err := cyclic.StartFunc(ctx, bus, func() (gocan.Frame, error) {
			calls++
			return gocan.Frame{}, nil
		}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		<-started
		cancel()
		<-task.Done()
		task.Stop()
		if !errors.Is(task.Err(), context.Canceled) {
			t.Fatalf("Err = %v, want context.Canceled", task.Err())
		}
		if calls != 2 || sends != 2 {
			t.Fatalf("calls/sends = %d/%d, want 2/2", calls, sends)
		}
	})
}

func TestStopWaitsForCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		calls, sends := 0, 0
		bus := &callbackBus{send: func(context.Context, gocan.Frame) error {
			sends++
			return nil
		}}
		task, err := cyclic.StartFunc(context.Background(), bus, func() (gocan.Frame, error) {
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
		stopped := make(chan struct{}, 2)
		for range 2 {
			go func() {
				task.Stop()
				stopped <- struct{}{}
			}()
		}
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("Stop returned before active work finished")
		case <-task.Done():
			t.Error("Done closed before active work finished")
		default:
		}
		close(release)
		<-stopped
		<-stopped
		<-task.Done()
		if err := task.Err(); err != nil {
			t.Fatalf("Err after Stop = %v, want nil", err)
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if calls != 2 || sends != 1 {
			t.Fatalf("calls/sends = %d/%d, want 2/1", calls, sends)
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
