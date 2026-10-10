package gocan_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tomrford/gocan"
)

func TestSendRetryLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		frame := gocan.Frame{ID: 0x123}
		calls := 0
		bus := &sendBus{done: make(chan struct{})}
		bus.send = func(context.Context, gocan.Frame) error {
			calls++
			return fmt.Errorf("adapter: %w", gocan.ErrTransmitQueueFull)
		}
		start := time.Now()
		if err := gocan.Send(context.Background(), bus, frame, 0); !errors.Is(err, gocan.ErrTransmitQueueFull) || calls != 1 || time.Now() != start {
			t.Fatalf("single attempt: %v, calls=%d, elapsed=%v", err, calls, time.Since(start))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Millisecond)
		defer cancel()
		err := gocan.Send(ctx, bus, frame, time.Second)
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, gocan.ErrTransmitQueueFull) || time.Since(start) != 3*time.Millisecond {
			t.Fatalf("caller deadline: %v after %v", err, time.Since(start))
		}
		start = time.Now()
		err = gocan.Send(context.Background(), bus, frame, 5*time.Millisecond)
		if errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, gocan.ErrTransmitQueueFull) || time.Since(start) != 5*time.Millisecond {
			t.Fatalf("retry budget: %v after %v", err, time.Since(start))
		}
		bus.send = func(context.Context, gocan.Frame) error { return gocan.ErrBusOff }
		start = time.Now()
		if err := gocan.Send(context.Background(), bus, frame, time.Second); !errors.Is(err, gocan.ErrBusOff) || time.Now() != start {
			t.Fatalf("fatal Send: %v after %v", err, time.Since(start))
		}
		cause := errors.New("caller stopped send")
		cancelled, cancelCause := context.WithCancelCause(context.Background())
		cancelCause(cause)
		bus.send = func(ctx context.Context, _ gocan.Frame) error { return ctx.Err() }
		if err := gocan.Send(cancelled, bus, frame, 0); !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
			t.Fatalf("first-attempt cancellation: %v", err)
		}
		for _, attempt := range []int{1, 2} {
			for _, result := range []error{nil, context.DeadlineExceeded, gocan.ErrBusOff} {
				calls = 0
				bus.send = func(ctx context.Context, got gocan.Frame) error {
					calls++
					if got != frame {
						t.Fatalf("retry changed frame: %+v", got)
					}
					if calls < attempt {
						return gocan.ErrTransmitQueueFull
					}
					time.Sleep(3 * time.Millisecond)
					if result == context.DeadlineExceeded {
						return ctx.Err()
					}
					return result
				}
				ctx, cancel := context.WithTimeoutCause(context.Background(), 2*time.Millisecond, cause)
				want := result
				if attempt == 2 {
					cancel()
					ctx = context.Background()
				}
				if result == context.DeadlineExceeded && attempt == 2 {
					want = gocan.ErrTransmitQueueFull
				} else if result == context.DeadlineExceeded {
					want = cause
				}
				err := gocan.Send(ctx, bus, frame, 2*time.Millisecond)
				cancel()
				if !errors.Is(err, want) || errors.Is(err, context.DeadlineExceeded) != (attempt == 1 && result == context.DeadlineExceeded) || calls != attempt {
					t.Fatalf("driver completion (attempt=%d, result=%v): %v, calls=%d", attempt, result, err, calls)
				}
			}
		}
		bus.send = func(context.Context, gocan.Frame) error { return gocan.ErrTransmitQueueFull }
		time.AfterFunc(2*time.Millisecond, func() { close(bus.done) })
		start = time.Now()
		if err := gocan.Send(context.Background(), bus, frame, time.Second); !errors.Is(err, gocan.ErrBusClosed) || time.Since(start) != 2*time.Millisecond {
			t.Fatalf("closed bus: %v after %v", err, time.Since(start))
		}
	})
}

type sendBus struct {
	gocan.Bus
	send func(context.Context, gocan.Frame) error
	done chan struct{}
}

func (bus *sendBus) Send(ctx context.Context, frame gocan.Frame) error {
	return bus.send(ctx, frame)
}
func (bus *sendBus) Done() <-chan struct{} { return bus.done }
func (bus *sendBus) Err() error            { return nil }
