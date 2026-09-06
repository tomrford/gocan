package j1939_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/j1939"
)

// Wire fixtures are specified independently of the active client's helpers.
// TP layouts/timers: Linux J1939 and python-can-j1939, pinned in .repos/.lock.
type activeBus struct {
	capture *gocan.Capture
	sent    chan gocan.Frame
	done    chan struct{}
	mu      sync.Mutex
	failure error
	delayID uint32
	delay   time.Duration
}

func newActiveBus() *activeBus {
	return &activeBus{capture: gocan.NewCapture(), sent: make(chan gocan.Frame, 1024), done: make(chan struct{})}
}
func (b *activeBus) ID() gocan.BusID         { return 1 }
func (b *activeBus) Name() string            { return "J1939 fixture" }
func (b *activeBus) Capture() *gocan.Capture { return b.capture }
func (b *activeBus) Done() <-chan struct{}   { return b.done }
func (b *activeBus) Err() error              { return nil }
func (b *activeBus) Close() error            { close(b.done); return nil }
func (b *activeBus) Send(ctx context.Context, f gocan.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	err := b.failure
	delayID, delay := b.delayID, b.delay
	b.mu.Unlock()
	if err != nil {
		return err
	}
	if f.ID == delayID {
		time.Sleep(delay)
	}
	if err := b.capture.Append(gocan.FrameEvent{Bus: 1, Timestamp: time.Now(), Direction: gocan.DirectionTransmit, Frame: f}); err != nil {
		return err
	}
	b.sent <- f
	return nil
}
func (b *activeBus) inject(t *testing.T, id uint32, data ...byte) {
	t.Helper()
	f, err := gocan.NewFrame(id, data, gocan.FrameExtended)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.capture.Append(gocan.FrameEvent{Bus: 1, Timestamp: time.Now(), Direction: gocan.DirectionReceive, Frame: f}); err != nil {
		t.Fatal(err)
	}
}
func wire(t *testing.T, b *activeBus, id uint32, data ...byte) {
	t.Helper()
	select {
	case f := <-b.sent:
		if f.ID != id || f.Flags != gocan.FrameExtended || !bytes.Equal(f.Data[:f.DataLength()], data) {
			t.Fatalf("wire = %#x % x; want %#x % x", f.ID, f.Data[:f.DataLength()], id, data)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("missing wire %#x % x", id, data)
	}
}
func quiet(t *testing.T, b *activeBus) {
	t.Helper()
	synctest.Wait()
	select {
	case f := <-b.sent:
		t.Fatalf("unexpected wire %#x % x", f.ID, f.Data[:f.DataLength()])
	default:
	}
}
func openActive(t *testing.T, b *activeBus) *j1939.Client {
	t.Helper()
	c, err := j1939.Open(context.Background(), b, j1939.Config{Name: 0x1234, Address: 0x80})
	if err != nil {
		t.Fatal(err)
	}
	wire(t, b, 0x18eeff80, 0x34, 0x12, 0, 0, 0, 0, 0, 0)
	t.Cleanup(c.Close)
	return c
}
func sendActive(c *j1939.Client, ctx context.Context, dst j1939.Address, data []byte) <-chan error {
	result := make(chan error, 1)
	go func() { result <- c.Send(ctx, 6, 0xfeca, dst, data) }()
	return result
}
func resultIs(t *testing.T, result <-chan error, want error) {
	t.Helper()
	select {
	case err := <-result:
		if !errors.Is(err, want) {
			t.Fatalf("result = %v, want %v", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing result")
	}
}

var activePayload = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}

func TestActiveClaimRequestAndAddressLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newActiveBus()
		start := time.Now()
		c := openActive(t, b)
		if time.Since(start) < 250*time.Millisecond || c.Address() != 0x80 {
			t.Fatal("used address before arbitration")
		}
		if err := c.Send(context.Background(), 3, 0xef00, 0x22, []byte{4, 5}); err != nil {
			t.Fatal(err)
		}
		wire(t, b, 0x0cef2280, 4, 5)
		if err := c.Request(context.Background(), 0x22, 0xfeca); err != nil {
			t.Fatal(err)
		}
		wire(t, b, 0x18ea2280, 0xca, 0xfe, 0)
		b.inject(t, 0x18eaff22, 0, 0xee, 0)
		wire(t, b, 0x18eeff80, 0x34, 0x12, 0, 0, 0, 0, 0, 0)
		// Higher NAME loses; our client must defend the configured address.
		b.inject(t, 0x18eeff80, 0x35, 0x12, 0, 0, 0, 0, 0, 0)
		wire(t, b, 0x18eeff80, 0x34, 0x12, 0, 0, 0, 0, 0, 0)
		r := sendActive(c, context.Background(), 0x22, activePayload)
		wire(t, b, 0x18ec2280, 0x10, 20, 0, 3, 3, 0xca, 0xfe, 0)
		b.inject(t, 0x18eeff80, 1, 0, 0, 0, 0, 0, 0, 0)
		resultIs(t, r, j1939.ErrAddressLost)
		wire(t, b, 0x18eefffe, 0x34, 0x12, 0, 0, 0, 0, 0, 0)
		if c.Address() != j1939.NullAddress {
			t.Fatal("lost address still usable")
		}
		if err := c.Request(context.Background(), 0xff, 0xfeca); !errors.Is(err, j1939.ErrAddressLost) {
			t.Fatal(err)
		}
		b.inject(t, 0x18eaff22, 0, 0xee, 0)
		wire(t, b, 0x18eefffe, 0x34, 0x12, 0, 0, 0, 0, 0, 0)
		quiet(t, b)
	})
}

func TestActiveBAMSpacingAndPassivePayload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newActiveBus()
		c := openActive(t, b)
		start := time.Now()
		r := sendActive(c, context.Background(), 0xff, activePayload)
		wire(t, b, 0x18ecff80, 0x20, 20, 0, 3, 0xff, 0xca, 0xfe, 0)
		wire(t, b, 0x1cebff80, 1, 1, 2, 3, 4, 5, 6, 7)
		if time.Since(start) < 50*time.Millisecond {
			t.Fatal("BAM sent first DT too early")
		}
		wire(t, b, 0x1cebff80, 2, 8, 9, 10, 11, 12, 13, 14)
		wire(t, b, 0x1cebff80, 3, 15, 16, 17, 18, 19, 20, 0xff)
		resultIs(t, r, nil)
		var decoder j1939.Decoder
		messages, diagnostics := decoder.PushBatch(b.capture.Frames())
		if len(diagnostics) != 0 {
			t.Fatal(diagnostics)
		}
		if len(messages) != 2 || !bytes.Equal(messages[1].Payload, activePayload) {
			t.Fatalf("decoded %v", messages)
		}
		var last time.Time
		for _, e := range b.capture.Frames() {
			if e.Frame.ID == 0x18ecff80 || e.Frame.ID == 0x1cebff80 {
				if !last.IsZero() && (e.Timestamp.Sub(last) < 50*time.Millisecond || e.Timestamp.Sub(last) > 200*time.Millisecond) {
					t.Fatal("BAM spacing", e.Timestamp.Sub(last))
				}
				last = e.Timestamp
			}
		}
	})
}

func TestActiveDirectedSendWindowsPauseRetransmitAndAcknowledgement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newActiveBus()
		c := openActive(t, b)
		r := sendActive(c, context.Background(), 0x22, activePayload)
		wire(t, b, 0x18ec2280, 0x10, 20, 0, 3, 3, 0xca, 0xfe, 0)
		resultIs(t, sendActive(c, context.Background(), 0x23, activePayload), j1939.ErrBusy)
		b.inject(t, 0x1cec8023, 0x11, 3, 1, 0xff, 0xff, 0xca, 0xfe, 0) // wrong peer
		time.Sleep(20 * time.Millisecond)
		quiet(t, b)
		b.inject(t, 0x1cec8022, 0x11, 0, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
		time.Sleep(500 * time.Millisecond)
		quiet(t, b)
		b.inject(t, 0x1cec8022, 0x11, 2, 1, 0xff, 0xff, 0xca, 0xfe, 0)
		wire(t, b, 0x1ceb2280, 1, 1, 2, 3, 4, 5, 6, 7)
		wire(t, b, 0x1ceb2280, 2, 8, 9, 10, 11, 12, 13, 14)
		b.inject(t, 0x1cec8022, 0x11, 2, 2, 0xff, 0xff, 0xca, 0xfe, 0)
		wire(t, b, 0x1ceb2280, 2, 8, 9, 10, 11, 12, 13, 14)
		wire(t, b, 0x1ceb2280, 3, 15, 16, 17, 18, 19, 20, 0xff)
		synctest.Wait()
		select {
		case err := <-r:
			t.Fatalf("completed without EOMA: %v", err)
		default:
		}
		// Passive observation can already complete before delivery is acknowledged.
		var decoder j1939.Decoder
		messages, diagnostics := decoder.PushBatch(b.capture.Frames())
		if len(diagnostics) != 1 || !errors.Is(diagnostics[0], j1939.ErrProtocol) || len(messages) != 2 || !bytes.Equal(messages[1].Payload, activePayload) {
			t.Fatalf("passive = %v, %v", messages, diagnostics)
		}
		b.inject(t, 0x1cec8022, 0x13, 20, 0, 3, 0xff, 0xca, 0xfe, 0)
		resultIs(t, r, nil)
	})
}

func TestActiveReceiveWindowsTimeoutAndRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newActiveBus()
		c := openActive(t, b)
		b.inject(t, 0x18ec8022, 0x10, 20, 0, 3, 2, 0xca, 0xfe, 0)
		wire(t, b, 0x1cec2280, 0x11, 2, 1, 0xff, 0xff, 0xca, 0xfe, 0)
		b.inject(t, 0x18ec8023, 0x10, 20, 0, 3, 2, 0xca, 0xfe, 0)
		wire(t, b, 0x1cec2380, 0xff, 1, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
		time.Sleep(900 * time.Millisecond) // first DT may legally take longer than T1
		quiet(t, b)
		b.inject(t, 0x1ceb8022, 1, 1, 2, 3, 4, 5, 6, 7)
		time.Sleep(760 * time.Millisecond)
		wire(t, b, 0x1cec2280, 0xff, 3, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
		b.inject(t, 0x1ceb8022, 2, 8, 9, 10, 11, 12, 13, 14)
		time.Sleep(10 * time.Millisecond)
		quiet(t, b)
		b.inject(t, 0x18ec8022, 0x10, 20, 0, 3, 2, 0xca, 0xfe, 0)
		wire(t, b, 0x1cec2280, 0x11, 2, 1, 0xff, 0xff, 0xca, 0xfe, 0)
		b.inject(t, 0x1ceb8022, 1, 1, 2, 3, 4, 5, 6, 7)
		b.inject(t, 0x1ceb8022, 2, 8, 9, 10, 11, 12, 13, 14)
		wire(t, b, 0x1cec2280, 0x11, 1, 3, 0xff, 0xff, 0xca, 0xfe, 0)
		b.inject(t, 0x1ceb8022, 3, 15, 16, 17, 18, 19, 20, 0xff)
		wire(t, b, 0x1cec2280, 0x13, 20, 0, 3, 0xff, 0xca, 0xfe, 0)
		if c.Address() != 0x80 {
			t.Fatal("transport error lost local claim")
		}
	})
}

func TestActiveSendFailuresCancelAndRecover(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout", "early_ack", "abort", "bad_cts", "send_failure"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := newActiveBus()
				c := openActive(t, b)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				r := sendActive(c, ctx, 0x22, activePayload)
				wire(t, b, 0x18ec2280, 0x10, 20, 0, 3, 3, 0xca, 0xfe, 0)
				want := j1939.ErrProtocol
				reason := byte(2)
				switch mode {
				case "cancel":
					cancel()
					want = context.Canceled
				case "timeout":
					time.Sleep(1260 * time.Millisecond)
					want = j1939.ErrTimeout
					reason = 3
				case "early_ack":
					b.inject(t, 0x1cec8022, 0x13, 20, 0, 3, 0xff, 0xca, 0xfe, 0)
				case "abort":
					b.inject(t, 0x1cec8022, 0xff, 2, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
				case "bad_cts":
					b.inject(t, 0x1cec8022, 0x11, 3, 2, 0xff, 0xff, 0xca, 0xfe, 0)
				case "send_failure":
					b.mu.Lock()
					b.failure = gocan.ErrTransmitQueueFull
					b.mu.Unlock()
					b.inject(t, 0x1cec8022, 0x11, 3, 1, 0xff, 0xff, 0xca, 0xfe, 0)
					want = gocan.ErrTransmitQueueFull
				}
				resultIs(t, r, want)
				if mode == "send_failure" {
					<-c.Done()
					if !errors.Is(c.Err(), want) {
						t.Fatal(c.Err())
					}
					return
				}
				if mode != "abort" {
					wire(t, b, 0x1cec2280, 0xff, reason, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
				}
				// A failed transaction must release transport ownership for a new one.
				r = sendActive(c, context.Background(), 0x22, activePayload)
				wire(t, b, 0x18ec2280, 0x10, 20, 0, 3, 3, 0xca, 0xfe, 0)
				c.Close()
				resultIs(t, r, j1939.ErrClosed)
				wire(t, b, 0x1cec2280, 0xff, 2, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
				quiet(t, b)
			})
		})
	}
}

func TestActiveCaptureLossAndBusClose(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(map[bool]string{false: "bus", true: "capture"}[clear], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := newActiveBus()
				c := openActive(t, b)
				r := sendActive(c, context.Background(), 0x22, activePayload)
				wire(t, b, 0x18ec2280, 0x10, 20, 0, 3, 3, 0xca, 0xfe, 0)
				synctest.Wait()
				want := gocan.ErrBusClosed
				if clear {
					b.capture.Clear()
					want = gocan.ErrCursorOutOfRange
				} else {
					b.Close()
				}
				resultIs(t, r, want)
				<-c.Done()
				if c.Address() != j1939.NullAddress {
					t.Fatal("terminal client retains address")
				}
			})
		})
	}
}

func TestActiveRejectsDataBeforeCTSAndMalformedReplacements(t *testing.T) {
	for _, mode := range []string{"before_first_cts", "before_next_cts", "replacement", "malformed_bam", "short_rts"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := newActiveBus()
				openActive(t, b)
				b.inject(t, 0x18ec8022, 0x10, 20, 0, 3, 2, 0xca, 0xfe, 0)
				if mode == "before_first_cts" {
					b.inject(t, 0x1ceb8022, 1, 1, 2, 3, 4, 5, 6, 7)
				}
				wire(t, b, 0x1cec2280, 0x11, 2, 1, 0xff, 0xff, 0xca, 0xfe, 0)
				switch mode {
				case "before_first_cts":
					wire(t, b, 0x1cec2280, 0xff, 7, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
				case "before_next_cts":
					b.inject(t, 0x1ceb8022, 1, 1, 2, 3, 4, 5, 6, 7)
					b.inject(t, 0x1ceb8022, 2, 8, 9, 10, 11, 12, 13, 14)
					b.inject(t, 0x1ceb8022, 3, 15, 16, 17, 18, 19, 20, 0xff)
					wire(t, b, 0x1cec2280, 0x11, 1, 3, 0xff, 0xff, 0xca, 0xfe, 0)
					wire(t, b, 0x1cec2280, 0xff, 7, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
				case "replacement":
					b.inject(t, 0x18ec8022, 0x10, 20, 0, 3, 2, 0xcb, 0xfe, 0)
					wire(t, b, 0x1cec2280, 0xff, 1, 0xff, 0xff, 0xff, 0xcb, 0xfe, 0)
				case "malformed_bam":
					b.inject(t, 0x18ec8022, 0x20, 20, 0, 3, 0xff, 0xca, 0xfe, 0)
					wire(t, b, 0x1cec2280, 0xff, 2, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
				case "short_rts":
					b.inject(t, 0x18ec8022, 0x10)
					wire(t, b, 0x1cec2280, 0xff, 2, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
				}
				b.inject(t, 0x1ceb8022, 1, 1, 2, 3, 4, 5, 6, 7)
				b.inject(t, 0x1ceb8022, 2, 8, 9, 10, 11, 12, 13, 14)
				b.inject(t, 0x1ceb8022, 3, 15, 16, 17, 18, 19, 20, 0xff)
				time.Sleep(10 * time.Millisecond)
				quiet(t, b)
				// Recovery must use a new RTS and cannot inherit old bytes or grants.
				b.inject(t, 0x18ec8022, 0x10, 9, 0, 2, 2, 0xca, 0xfe, 0)
				wire(t, b, 0x1cec2280, 0x11, 2, 1, 0xff, 0xff, 0xca, 0xfe, 0)
				b.inject(t, 0x1ceb8022, 1, 1, 2, 3, 4, 5, 6, 7)
				b.inject(t, 0x1ceb8022, 2, 8, 9, 0xff, 0xff, 0xff, 0xff, 0xff)
				wire(t, b, 0x1cec2280, 0x13, 9, 0, 2, 0xff, 0xca, 0xfe, 0)
			})
		})
	}
}

func TestActiveTimelyFrameAtPollingBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newActiveBus()
		openActive(t, b)
		b.inject(t, 0x18ec8022, 0x10, 9, 0, 2, 2, 0xca, 0xfe, 0)
		wire(t, b, 0x1cec2280, 0x11, 2, 1, 0xff, 0xff, 0xca, 0xfe, 0)
		// The frame arrives within T2 but is processed at the deadline's tick.
		time.Sleep(1249 * time.Millisecond)
		b.inject(t, 0x1ceb8022, 1, 1, 2, 3, 4, 5, 6, 7)
		time.Sleep(5 * time.Millisecond)
		quiet(t, b)
		b.inject(t, 0x1ceb8022, 2, 8, 9, 0xff, 0xff, 0xff, 0xff, 0xff)
		wire(t, b, 0x1cec2280, 0x13, 9, 0, 2, 0xff, 0xca, 0xfe, 0)
	})
}

func TestActiveMaximumPayloadAndFirstDTTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newActiveBus()
		c := openActive(t, b)
		if err := c.Send(context.Background(), 6, 0xfeca, 0xff, make([]byte, 1786)); !errors.Is(err, j1939.ErrUnsupported) {
			t.Fatal(err)
		}
		b.inject(t, 0x18ec8022, 0x10, 0xf9, 6, 0xff, 0xff, 0xca, 0xfe, 0)
		wire(t, b, 0x1cec2280, 0x11, 0xff, 1, 0xff, 0xff, 0xca, 0xfe, 0)
		time.Sleep(1250 * time.Millisecond)
		wire(t, b, 0x1cec2280, 0xff, 3, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
		b.inject(t, 0x18ec8022, 0x10, 0xf9, 6, 0xff, 0xff, 0xca, 0xfe, 0)
		wire(t, b, 0x1cec2280, 0x11, 0xff, 1, 0xff, 0xff, 0xca, 0xfe, 0)
		for seq := 1; seq <= 255; seq++ {
			b.inject(t, 0x1ceb8022, byte(seq), 1, 2, 3, 4, 5, 6, 7)
		}
		wire(t, b, 0x1cec2280, 0x13, 0xf9, 6, 0xff, 0xff, 0xca, 0xfe, 0)
		payload := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, 255)
		r := sendActive(c, context.Background(), 0x22, payload)
		wire(t, b, 0x18ec2280, 0x10, 0xf9, 6, 0xff, 0xff, 0xca, 0xfe, 0)
		b.inject(t, 0x1cec8022, 0x11, 0xff, 1, 0xff, 0xff, 0xca, 0xfe, 0)
		for seq := 1; seq <= 255; seq++ {
			wire(t, b, 0x1ceb2280, byte(seq), 1, 2, 3, 4, 5, 6, 7)
		}
		b.inject(t, 0x1cec8022, 0x13, 0xf9, 6, 0xff, 0xff, 0xca, 0xfe, 0)
		resultIs(t, r, nil)
	})
}

func TestActiveClaimCancellationAndLossWhileOpening(t *testing.T) {
	for _, cancelClaim := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost", true: "cancelled"}[cancelClaim], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := newActiveBus()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() {
					c, err := j1939.Open(ctx, b, j1939.Config{Name: 0x1234, Address: 0x80})
					if c != nil {
						c.Close()
					}
					result <- err
				}()
				wire(t, b, 0x18eeff80, 0x34, 0x12, 0, 0, 0, 0, 0, 0)
				want := j1939.ErrAddressLost
				if cancelClaim {
					cancel()
					want = context.Canceled
				} else {
					b.inject(t, 0x18eeff80, 1, 0, 0, 0, 0, 0, 0, 0)
					wire(t, b, 0x18eefffe, 0x34, 0x12, 0, 0, 0, 0, 0, 0)
				}
				resultIs(t, result, want)
				time.Sleep(300 * time.Millisecond)
				quiet(t, b)
			})
		})
	}
}

func TestActiveResponseArrivingDuringNativeSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newActiveBus()
		c := openActive(t, b)
		b.inject(t, 0x18ec8022, 0x10, 9, 0, 2, 2, 0xca, 0xfe, 0)
		wire(t, b, 0x1cec2280, 0x11, 2, 1, 0xff, 0xff, 0xca, 0xfe, 0)
		time.Sleep(1100 * time.Millisecond)
		b.mu.Lock()
		b.delayID = 0x18ef2380
		b.delay = 200 * time.Millisecond
		b.mu.Unlock()
		sent := make(chan error, 1)
		go func() { sent <- c.Send(context.Background(), 6, 0xef00, 0x23, []byte{42}) }()
		synctest.Wait() // the native send is blocked, but incoming capture remains live
		time.Sleep(100 * time.Millisecond)
		b.inject(t, 0x1ceb8022, 1, 1, 2, 3, 4, 5, 6, 7)
		resultIs(t, sent, nil)
		wire(t, b, 0x18ef2380, 42)
		time.Sleep(10 * time.Millisecond)
		quiet(t, b)
		b.inject(t, 0x1ceb8022, 2, 8, 9, 0xff, 0xff, 0xff, 0xff, 0xff)
		wire(t, b, 0x1cec2280, 0x13, 9, 0, 2, 0xff, 0xca, 0xfe, 0)
	})
}

func TestActiveCancellationWaitsForAbort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newActiveBus()
		c := openActive(t, b)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := sendActive(c, ctx, 0x22, activePayload)
		wire(t, b, 0x18ec2280, 0x10, 20, 0, 3, 3, 0xca, 0xfe, 0)
		b.mu.Lock()
		b.delayID = 0x1cec2280
		b.delay = 100 * time.Millisecond
		b.mu.Unlock()
		cancel()
		time.Sleep(50 * time.Millisecond)
		select {
		case err := <-r:
			t.Fatalf("returned before native abort completed: %v", err)
		default:
		}
		resultIs(t, r, context.Canceled)
		wire(t, b, 0x1cec2280, 0xff, 2, 0xff, 0xff, 0xff, 0xca, 0xfe, 0)
	})
}

func TestActivePeerClaimChangeAndIndependentTransport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newActiveBus()
		c := openActive(t, b)
		b.inject(t, 0x18eeff22, 0x45, 0x23, 0, 0, 0, 0, 0, 0)
		time.Sleep(10 * time.Millisecond)
		r := sendActive(c, context.Background(), 0x22, activePayload)
		wire(t, b, 0x18ec2280, 0x10, 20, 0, 3, 3, 0xca, 0xfe, 0)
		// Reception from another peer remains independent of this send.
		b.inject(t, 0x18ec8023, 0x10, 9, 0, 2, 2, 0xca, 0xfe, 0)
		wire(t, b, 0x1cec2380, 0x11, 2, 1, 0xff, 0xff, 0xca, 0xfe, 0)
		// The transmit peer moves its NAME; do not send an abort to its old address.
		b.inject(t, 0x18eeff24, 0x45, 0x23, 0, 0, 0, 0, 0, 0)
		resultIs(t, r, j1939.ErrProtocol)
		b.inject(t, 0x1ceb8023, 1, 1, 2, 3, 4, 5, 6, 7)
		b.inject(t, 0x1ceb8023, 2, 8, 9, 0xff, 0xff, 0xff, 0xff, 0xff)
		wire(t, b, 0x1cec2380, 0x13, 9, 0, 2, 0xff, 0xca, 0xfe, 0)
		quiet(t, b)
	})
}

func TestActiveClientsOverVirtualBus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var network virtual.Network
		captures := []*gocan.Capture{gocan.NewCapture(), gocan.NewCapture()}
		var clients []*j1939.Client
		for i, address := range []j1939.Address{0x80, 0x22} {
			bus, err := network.Open(context.Background(), captures[i], virtual.Config{ID: gocan.BusID(i + 1), Name: "J1939"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { bus.Close() })
			client, err := j1939.Open(context.Background(), bus, j1939.Config{Name: j1939.Name(i + 1), Address: address})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)
			clients = append(clients, client)
		}
		for i, destination := range []j1939.Address{0x22, 0x80} {
			if err := clients[i].Send(context.Background(), 6, 0xfeca, destination, activePayload); err != nil {
				t.Fatal(err)
			}
		}
		for _, capture := range captures {
			var decoder j1939.Decoder
			messages, diagnostics := decoder.PushBatch(capture.Frames())
			if len(diagnostics) != 0 {
				t.Fatal(diagnostics)
			}
			var received, sent int
			for _, message := range messages {
				if message.PGN == 0xfeca {
					if !bytes.Equal(message.Payload, activePayload) {
						t.Fatal("corrupted virtual payload")
					}
					if message.Direction == gocan.DirectionReceive {
						received++
					} else {
						sent++
					}
				}
			}
			if received != 1 || sent != 1 {
				t.Fatalf("virtual complete messages: received %d, sent %d", received, sent)
			}
		}
	})
}

func TestActiveClaimConflictArrivingDuringDefence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newActiveBus()
		result := make(chan error, 1)
		go func() {
			c, err := j1939.Open(context.Background(), b, j1939.Config{Name: 0x1234, Address: 0x80})
			if c != nil {
				c.Close()
			}
			result <- err
		}()
		wire(t, b, 0x18eeff80, 0x34, 0x12, 0, 0, 0, 0, 0, 0)
		time.Sleep(240 * time.Millisecond)
		b.mu.Lock()
		b.delayID = 0x18eeff80
		b.delay = 100 * time.Millisecond
		b.mu.Unlock()
		b.inject(t, 0x18eeff80, 0x35, 0x12, 0, 0, 0, 0, 0, 0)
		time.Sleep(9 * time.Millisecond)
		// The winning claim arrives within the initial arbitration interval while
		// our defence send is blocked. Open must not report success ahead of it.
		b.inject(t, 0x18eeff80, 1, 0, 0, 0, 0, 0, 0, 0)
		wire(t, b, 0x18eeff80, 0x34, 0x12, 0, 0, 0, 0, 0, 0)
		wire(t, b, 0x18eefffe, 0x34, 0x12, 0, 0, 0, 0, 0, 0)
		resultIs(t, result, j1939.ErrAddressLost)
	})
}
