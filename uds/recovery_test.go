package uds_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/isotp"
	"github.com/tomrford/gocan/uds"
)

func TestResponseRecovery(t *testing.T) {
	const ms = time.Millisecond
	type reply struct {
		after time.Duration
		data  []byte
	}
	for _, test := range []struct {
		name           string
		recovery       time.Duration
		replies        []reply
		deadline       time.Duration
		cancelAt       time.Duration
		closeAt        time.Duration
		suppressed     bool
		cancelBuffered bool
		want           error
		elapsed        time.Duration
	}{
		{name: "timely", recovery: 100 * ms, replies: []reply{{200 * ms, []byte{2, 0x76, 1}}}, elapsed: 200 * ms},
		{name: "late", recovery: 100 * ms, replies: []reply{{585 * ms, []byte{2, 0x76, 1}}}, elapsed: 585 * ms},
		{name: "disabled", replies: []reply{{585 * ms, []byte{2, 0x76, 1}}}, want: uds.ErrP2Timeout, elapsed: 500 * ms},
		{name: "bounded silence", recovery: 100 * ms, want: uds.ErrP2Timeout, elapsed: 600 * ms},
		{name: "too late", recovery: 100 * ms, replies: []reply{{650 * ms, []byte{2, 0x76, 1}}}, want: uds.ErrP2Timeout, elapsed: 600 * ms},
		{name: "suppressed silence", recovery: 100 * ms, suppressed: true, elapsed: 600 * ms},
		{name: "repeated pending during recovery", recovery: 100 * ms, replies: []reply{{550 * ms, []byte{3, 0x7f, 0x36, 0x78}}, {250 * ms, []byte{3, 0x7f, 0x36, 0x78}}, {250 * ms, []byte{2, 0x76, 1}}}, elapsed: 1050 * ms},
		{name: "pending silence", recovery: 100 * ms, replies: []reply{{550 * ms, []byte{3, 0x7f, 0x36, 0x78}}}, want: uds.ErrP2StarTimeout, elapsed: 850 * ms},
		{name: "caller deadline", recovery: 100 * ms, deadline: 575 * ms, want: context.DeadlineExceeded, elapsed: 575 * ms},
		{name: "caller cancellation", recovery: 100 * ms, cancelAt: 550 * ms, want: context.Canceled, elapsed: 550 * ms},
		{name: "buffered response after cancellation", recovery: 100 * ms, cancelBuffered: true, replies: []reply{{550 * ms, []byte{0x10, 8, 0x76, 1, 2, 3, 4, 5}}}, want: context.Canceled, elapsed: 550 * ms},
		{name: "bus closes", recovery: 100 * ms, closeAt: 550 * ms, want: gocan.ErrBusClosed, elapsed: 550 * ms},
		{name: "malformed reply", recovery: 100 * ms, replies: []reply{{550 * ms, []byte{2, 0x7f, 0x36}}}, want: uds.ErrInvalidResponse, elapsed: 550 * ms},
		{name: "incomplete response", recovery: 100 * ms, replies: []reply{{550 * ms, []byte{0x10, 8, 0x76, 1, 2, 3, 4, 5}}}, want: isotp.ErrConsecutiveFrameTimeout, elapsed: 575 * ms},
		{name: "segmented response outlives recovery", recovery: 100 * ms, replies: []reply{{590 * ms, []byte{0x10, 8, 0x76, 1, 2, 3, 4, 5}}, {20 * ms, []byte{0x21, 6, 7}}}, elapsed: 610 * ms},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				capture := gocan.NewCapture()
				var network virtual.Network
				tester, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
				if err != nil {
					t.Fatal(err)
				}
				defer tester.Close()
				ecu, err := network.Open(context.Background(), capture, virtual.Config{ID: 2, Name: "ECU"})
				if err != nil {
					t.Fatal(err)
				}
				defer ecu.Close()
				bus := &requestCountingBus{Bus: tester}
				link, err := isotp.New(bus, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8, ConsecutiveFrameTimeout: 25 * ms})
				if err != nil {
					t.Fatal(err)
				}
				config := uds.Config{P2Timeout: 500 * ms, P2StarTimeout: 200 * ms, ResponseRecoveryTimeout: test.recovery}
				client, err := uds.New(link, config)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if test.cancelBuffered {
					bus.cancelOnFlow = cancel
				}
				if test.deadline > 0 {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, test.deadline)
					defer stop()
				}
				result := make(chan error, 1)
				start := time.Now()
				go func() {
					request := uds.Request{Service: 0x36, Data: []byte{1}}
					var err error
					if test.suppressed {
						_, err = client.DoSuppressed(ctx, request)
					} else {
						var response uds.Response
						response, err = client.Do(ctx, request)
						if err == nil && (response.Service != 0x36 || len(response.Data) == 0 || response.Data[0] != 1) {
							t.Errorf("unexpected response: %+v", response)
						}
					}
					result <- err
				}()
				synctest.Wait() // The request is sent and its first response wait has started.
				go func() {
					for _, reply := range test.replies {
						time.Sleep(reply.after)
						frame, err := gocan.NewFrame(0x7e8, reply.data, 0)
						if err != nil {
							t.Error(err)
							return
						}
						if err := ecu.Send(context.Background(), frame); err != nil {
							t.Error(err)
							return
						}
					}
				}()
				if test.cancelAt > 0 {
					time.AfterFunc(test.cancelAt, cancel)
				}
				if test.closeAt > 0 {
					time.AfterFunc(test.closeAt, func() { _ = tester.Close() })
				}
				if err := <-result; !errors.Is(err, test.want) {
					t.Fatalf("Do = %v, want %v", err, test.want)
				}
				if elapsed := time.Since(start); elapsed != test.elapsed {
					t.Fatalf("elapsed = %v, want %v", elapsed, test.elapsed)
				}
				if bus.requests != 1 {
					t.Fatalf("sent %d requests, want one", bus.requests)
				}
				// Let scheduled late replies finish before closing either bus.
				time.Sleep(time.Second)
			})
		})
	}
}

type requestCountingBus struct {
	gocan.Bus
	requests     int
	cancelOnFlow context.CancelFunc
}

func (bus *requestCountingBus) Send(ctx context.Context, frame gocan.Frame) error {
	if frame.Data[0]>>4 != 3 {
		bus.requests++
	}
	if err := bus.Bus.Send(ctx, frame); err != nil {
		return err
	}
	if frame.Data[0]>>4 == 3 && bus.cancelOnFlow != nil {
		// Queue the final segment before cancelling. Capture reads buffered frames
		// first, so the UDS caller must still reject this completed response.
		final, err := gocan.NewFrame(0x7e8, []byte{0x21, 6, 7}, 0)
		if err != nil {
			return err
		}
		if err := bus.Capture().RecordFrame(gocan.FrameEvent{Bus: bus.ID(), Direction: gocan.DirectionReceive, Frame: final}); err != nil {
			return err
		}
		bus.cancelOnFlow()
	}
	return nil
}
