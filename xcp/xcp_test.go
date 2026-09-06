package xcp_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/xcp"
)

// Packet expectations are literal XCP wire fixtures, independent of the client
// encoder. CONNECT, status and memory layouts are cross-checked against Vector
// xcp.h and pyXCP types.py/master.py at the revisions cited in command.go.
func TestReadLifecycle(t *testing.T) {
	for _, fd := range []bool{false, true} {
		t.Run(fmt.Sprintf("FD=%v", fd), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, fd)
				connect := []byte{0xff, 5, 0x80, 8, 8, 0, 1, 1}
				mta := []byte{0xf6, 0, 0, 7, 0x78, 0x56, 0x34, 0x12}
				short := []byte{0xf4, 3, 0, 7, 0x78, 0x56, 0x34, 0x12}
				status := []byte{0xff, 4, 1, 0, 0x34, 0x12}
				var order binary.ByteOrder = binary.LittleEndian
				if fd {
					// FD padding capacity is 64; the ECU's CTO limit is only 12.
					connect = []byte{0xff, 5, 0x81, 12, 0, 64, 1, 1}
					mta = []byte{0xf6, 0, 0, 7, 0x12, 0x34, 0x56, 0x78}
					short = []byte{0xf4, 3, 0, 7, 0x12, 0x34, 0x56, 0x78}
					status = []byte{0xff, 4, 1, 0, 0x12, 0x34}
					order = binary.BigEndian
				}
				done := run(func() error {
					caps, err := r.client.Connect(r.ctx)
					if err == nil && (caps.ByteOrder != order || caps.AddressGranularity != 1 || caps.Resources != 5) {
						return fmt.Errorf("capabilities: %+v", caps)
					}
					return err
				})
				r.expect(0xff, 0)
				r.reply(connect...)
				r.finish(done)
				done = run(func() error {
					info, err := r.client.CommunicationInfo(r.ctx)
					if err == nil && info != (xcp.CommunicationInfo{Mode: 3, MaxBlockSize: 9, MinSeparationTime: 10, QueueSize: 2, DriverVersion: 1}) {
						return fmt.Errorf("communication: %+v", info)
					}
					return err
				})
				r.expect(0xfb)
				r.reply(0xff, 0, 3, 0, 9, 10, 2, 1)
				r.finish(done)
				done = run(func() error {
					value, err := r.client.Status(r.ctx)
					if err == nil && value != (xcp.Status{Session: 4, Protection: 1, ConfigurationID: 0x1234}) {
						return fmt.Errorf("status: %+v", value)
					}
					return err
				})
				r.expect(0xfd)
				r.reply(0, 0x42)          // DAQ
				r.reply(0xfc, 0x01, 0x42) // SERV
				r.reply(0xfd, 0xfe, 0x42) // user event
				r.reply(status...)
				r.finish(done)
				address := xcp.Address{Extension: 7, Value: 0x12345678}
				done = run(func() error {
					data, err := r.client.Read(r.ctx, address, 14)
					if err == nil && !bytes.Equal(data, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 0, 0xaa, 0}) {
						return fmt.Errorf("read: %x", data)
					}
					return err
				})
				r.expect(mta...)
				r.reply(0xff)
				if fd {
					r.expect(0xf5, 11)
					r.reply(0xff, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11)
					r.expect(0xf5, 3)
					r.reply(0xff, 0, 0xaa, 0)
				} else {
					r.expect(0xf5, 7)
					r.reply(0xff, 1, 2, 3, 4, 5, 6, 7)
					r.expect(0xf5, 7)
					r.reply(0xff, 8, 9, 10, 11, 0, 0xaa, 0)
				}
				r.finish(done)
				done = run(func() error {
					data, err := r.client.ShortUpload(r.ctx, address, 3)
					if err == nil && !bytes.Equal(data, []byte{0, 0xaa, 0}) {
						return fmt.Errorf("short upload: %x", data)
					}
					return err
				})
				r.expect(short...)
				r.reply(0xff, 0, 0xaa, 0)
				r.finish(done)
				done = run(func() error { return r.client.Disconnect(r.ctx) })
				r.expect(0xfe)
				r.reply(0xff)
				r.finish(done)
				if _, err := r.client.Read(r.ctx, address, 1); !errors.Is(err, xcp.ErrNotConnected) {
					t.Fatalf("read after disconnect: %v", err)
				}
				// The client never removes asynchronous packets from the shared trace.
				for _, pid := range []byte{0, 0xfc, 0xfd} {
					found := false
					for _, event := range r.capture.Series(r.receiveKey()) {
						if event.Frame.Data[0] == pid {
							found = true
						}
					}
					if !found {
						t.Fatalf("missing async PID %x from capture", pid)
					}
				}
			})
		})
	}
}

func TestRecoveryAndCaptureLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, false)
		r.connect()
		done := run(func() error { _, err := r.client.Status(r.ctx); return err })
		r.expect(0xfd)
		// A continuous stream of DAQ/user events does not restart the timer.
		for i := 0; i < 5; i++ {
			r.reply(0, byte(i))
			r.reply(0xfd, 0xfe)
			time.Sleep(5 * time.Millisecond)
		}
		if err := <-done; !errors.Is(err, xcp.ErrTimeout) {
			t.Fatalf("DAQ timeout: %v", err)
		}
		if _, err := r.client.Status(r.ctx); !errors.Is(err, xcp.ErrSynchronizationRequired) {
			t.Fatalf("uncertain session: %v", err)
		}
		done = run(func() error { return r.client.Synchronize(r.ctx) })
		r.expect(0xfc)
		r.reply(0xff, 0, 0, 0, 0, 0) // old GET_STATUS response
		r.reply(0xfe, 0x22)          // another old error is not ERR_CMD_SYNCH
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("SYNCH accepted stale response: %v", err)
		default:
		}
		r.reply(0xfe, 0)
		r.finish(done)
		done = run(func() error { _, err := r.client.Status(r.ctx); return err })
		r.expect(0xfd)
		r.reply(0xff, 1) // truncated final reply
		if err := <-done; !errors.Is(err, xcp.ErrInvalidResponse) {
			t.Fatalf("malformed: %v", err)
		}
		done = run(func() error { return r.client.Synchronize(r.ctx) })
		r.expect(0xfc)
		time.Sleep(21 * time.Millisecond)
		if err := <-done; !errors.Is(err, xcp.ErrTimeout) {
			t.Fatalf("failed SYNCH: %v", err)
		}
		if _, err := r.client.Status(r.ctx); !errors.Is(err, xcp.ErrSynchronizationRequired) {
			t.Fatalf("after failed SYNCH: %v", err)
		}
		r.reply(0xfe, 0) // The outstanding SYNCH can complete between calls.
		if err := r.client.Synchronize(r.ctx); err != nil {
			t.Fatal(err)
		}
		done = run(func() error { _, err := r.client.Status(r.ctx); return err })
		r.expect(0xfd)
		// Safe pruning keeps the active command boundary; Clear deliberately loses it.
		synctest.Wait() // Exercise Clear while Capture.Next is already parked.
		if err := r.capture.Prune(r.client.RetentionCursor()); err != nil {
			t.Fatal(err)
		}
		r.capture.Clear()
		r.cursor = gocan.Cursor{}
		r.reply(0xff, 0, 0, 0, 0, 0)
		if err := <-done; !errors.Is(err, gocan.ErrCursorOutOfRange) {
			t.Fatalf("capture loss: %v", err)
		}
		r.synchronize()
		done = run(func() error { _, err := r.client.Status(r.ctx); return err })
		r.expect(0xfd)
		r.reply(0xff, 0, 0, 0, 0, 0)
		r.finish(done)
		if r.client.RetentionCursor() != r.capture.End() {
			t.Fatal("idle client retains history")
		}
	})
}

func TestSynchronizeRetainsOneBarrier(t *testing.T) {
	for _, clearCapture := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear=%v", clearCapture), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, false)
				r.connect()
				done := run(func() error { return r.client.Synchronize(r.ctx) })
				r.expect(0xfc)
				time.Sleep(21 * time.Millisecond)
				if err := <-done; !errors.Is(err, xcp.ErrTimeout) {
					t.Fatal(err)
				}
				cursor := r.client.RetentionCursor()
				if err := r.capture.Prune(cursor); err != nil {
					t.Fatal(err)
				}
				if clearCapture {
					r.capture.Clear()
					r.reply(0xfe, 0)
					if err := r.client.Synchronize(r.ctx); !errors.Is(err, gocan.ErrCursorOutOfRange) {
						t.Fatalf("lost barrier: %v", err)
					}
					return
				}
				// Repeated waits must not send additional indistinguishable markers.
				done = run(func() error { return r.client.Synchronize(r.ctx) })
				synctest.Wait()
				if r.client.RetentionCursor() != cursor {
					t.Fatal("retry reset the outstanding barrier cursor")
				}
				r.reply(0xfe, 0)
				r.finish(done)
				done = run(func() error {
					status, err := r.client.Status(r.ctx)
					if err == nil && status.Session != 0x22 {
						return fmt.Errorf("unexpected status: %+v", status)
					}
					return err
				})
				r.expect(0xfd) // An extra SYNCH would appear here before GET_STATUS.
				r.reply(0xff, 0x22, 0, 0, 0, 0)
				r.finish(done)
			})
		})
	}
}

func TestReadIgnoresDTOLimits(t *testing.T) {
	for _, maxDTO := range []byte{0, 1, 64} {
		t.Run(fmt.Sprintf("DTO=%d", maxDTO), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, true)
				r.client.Close()
				r.config.TransmitDataLength = 8
				r.config.PadFrames = false
				var err error
				r.client, err = xcp.New(r.tester, r.config)
				if err != nil {
					t.Fatal(err)
				}
				defer r.client.Close()
				done := run(func() error {
					caps, err := r.client.Connect(r.ctx)
					if err == nil && caps.MaxDTO != uint16(maxDTO) {
						return fmt.Errorf("MAX_DTO lost: %+v", caps)
					}
					return err
				})
				r.expect(0xff, 0)
				resources := byte(1) // Memory access without DAQ.
				if maxDTO == 64 {
					resources |= 4
				}
				r.reply(0xff, resources, 0, 8, maxDTO, 0, 1, 1)
				r.finish(done)
				done = run(func() error {
					data, err := r.client.ShortUpload(r.ctx, xcp.Address{Value: 0x1000}, 1)
					if err == nil && !bytes.Equal(data, []byte{0x42}) {
						return fmt.Errorf("read: %x", data)
					}
					return err
				})
				r.expect(0xf4, 1, 0, 0, 0, 0x10, 0, 0)
				if maxDTO == 64 {
					r.reply(make([]byte, 64)...) // DAQ may exceed command capacity.
				}
				r.reply(0xff, 0x42)
				r.finish(done)
			})
		})
	}
}

func TestPendingAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, false)
		r.connect()
		ctx, cancel := context.WithTimeout(r.ctx, 45*time.Millisecond)
		defer cancel()
		done := run(func() error { _, err := r.client.Status(ctx); return err })
		r.expect(0xfd)
		for i := 0; i < 4; i++ {
			time.Sleep(10 * time.Millisecond)
			r.reply(0xfd, 5)
		}
		time.Sleep(6 * time.Millisecond)
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("pending bypassed caller deadline: %v", err)
		}
		r.synchronize()
		ctx, cancel = context.WithCancel(r.ctx)
		done = run(func() error { _, err := r.client.Status(ctx); return err })
		r.expect(0xfd)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel accepted command: %v", err)
		}
		if _, err := r.client.Status(r.ctx); !errors.Is(err, xcp.ErrSynchronizationRequired) {
			t.Fatalf("cancel lost uncertainty: %v", err)
		}
		r.synchronize()
		// Cancelled before sending: no synchronisation is needed.
		if _, err := r.client.Status(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		done = run(func() error { _, err := r.client.Status(r.ctx); return err })
		r.expect(0xfd)
		r.reply(0xff, 0, 0, 0, 0, 0)
		r.finish(done)
	})
}

func TestCompoundReadOwnershipAndPartialProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, false)
		r.connect()
		done := run(func() error {
			data, err := r.client.Read(r.ctx, xcp.Address{Value: 0x1000}, 10)
			var negative *xcp.NegativeResponseError
			if !errors.As(err, &negative) || negative.Command != xcp.CommandUpload || negative.Code != 0x24 || !bytes.Equal(data, []byte{1, 2, 3, 4, 5, 6, 7}) {
				return fmt.Errorf("partial read %x: %v", data, err)
			}
			return nil
		})
		r.expect(0xf6, 0, 0, 0, 0, 0x10, 0, 0)
		ctx, cancel := context.WithCancel(r.ctx)
		queuedRead := run(func() error { _, err := r.client.Read(ctx, xcp.Address{Value: 0x2000}, 1); return err })
		synctest.Wait()
		cancel()
		if err := <-queuedRead; !errors.Is(err, context.Canceled) {
			t.Fatalf("queued read: %v", err)
		}
		queuedRaw := run(func() error { _, err := r.client.Do(r.ctx, xcp.Request{Command: xcp.CommandGetStatus}); return err })
		synctest.Wait()
		r.reply(0xff)
		r.expect(0xf5, 7)
		r.reply(0xff, 1, 2, 3, 4, 5, 6, 7)
		r.expect(0xf5, 3)
		r.reply(0xfe, 0x24)
		r.finish(done)
		// The raw call only reaches the bus after the whole read releases ownership.
		r.expect(0xfd)
		r.reply(0xff, 0, 0, 0, 0, 0)
		r.finish(queuedRaw)
	})
}

func TestCloseAndBusLoss(t *testing.T) {
	for _, closeBus := range []bool{false, true} {
		t.Run(fmt.Sprintf("bus=%v", closeBus), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, false)
				r.connect()
				first := run(func() error { _, err := r.client.Status(r.ctx); return err })
				r.expect(0xfd)
				queued := run(func() error { _, err := r.client.Status(r.ctx); return err })
				synctest.Wait()
				want := xcp.ErrClosed
				if closeBus {
					want = gocan.ErrBusClosed
					_ = r.tester.Close()
				} else {
					r.client.Close()
					r.client.Close()
				}
				for _, done := range []<-chan error{first, queued} {
					if err := <-done; !errors.Is(err, want) {
						t.Fatalf("close: %v, want %v", err, want)
					}
				}
				if !closeBus {
					select {
					case <-r.tester.Done():
						t.Fatal("client closed bus")
					default:
					}
				}
			})
		})
	}
}

func TestRawSessionStateAndReadValidation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, false)
		done := run(func() error {
			_, err := r.client.Do(r.ctx, xcp.Request{Command: xcp.CommandConnect, Data: []byte{0}})
			return err
		})
		r.expect(0xff, 0)
		r.reply(0xff, 1, 2, 8, 8, 0, 1, 1)
		r.finish(done) // WORD granularity
		if _, err := r.client.Read(r.ctx, xcp.Address{}, 1); !errors.Is(err, xcp.ErrUnsupported) {
			t.Fatalf("word read: %v", err)
		}
		r.connect() // raw CONNECT changed the same trusted session state
		for _, test := range []struct {
			address uint32
			length  int
		}{{0, -1}, {0xfffffff0, 32}} {
			if _, err := r.client.Read(r.ctx, xcp.Address{Value: test.address}, test.length); err == nil {
				t.Fatal("accepted invalid range")
			}
		}
		if _, err := r.client.ShortUpload(r.ctx, xcp.Address{}, 8); !errors.Is(err, xcp.ErrUnsupported) {
			t.Fatal(err)
		}
		if _, err := r.client.Do(r.ctx, xcp.Request{Command: xcp.CommandUpload, Data: []byte{8}}); !errors.Is(err, xcp.ErrUnsupported) {
			t.Fatalf("raw block upload: %v", err)
		}
		if data, err := r.client.Read(r.ctx, xcp.Address{}, 0); err != nil || len(data) != 0 {
			t.Fatalf("empty read: %x %v", data, err)
		}
		if _, err := r.client.CommunicationInfo(r.ctx); !errors.Is(err, xcp.ErrUnsupported) {
			t.Fatalf("unadvertised information: %v", err)
		}
		// An unmodelled USER_CMD could change ECU state; the raw result is retained
		// but typed reads must not continue using pre-command capabilities.
		done = run(func() error {
			response, err := r.client.Do(r.ctx, xcp.Request{Command: 0xf1, Data: []byte{0}})
			if err == nil && !bytes.Equal(response.Data, []byte{0, 0xaa, 0}) {
				return fmt.Errorf("raw data: %x", response.Data)
			}
			return err
		})
		r.expect(0xf1, 0)
		r.reply(0xff, 0, 0xaa, 0)
		r.finish(done)
		if _, err := r.client.Read(r.ctx, xcp.Address{}, 1); !errors.Is(err, xcp.ErrNotConnected) {
			t.Fatalf("stale capabilities: %v", err)
		}
		r.connect()
		done = run(func() error { _, err := r.client.Do(r.ctx, xcp.Request{Command: xcp.CommandDisconnect}); return err })
		r.expect(0xfe)
		r.reply(0xff)
		r.finish(done)
		if _, err := r.client.Status(r.ctx); !errors.Is(err, xcp.ErrNotConnected) {
			t.Fatalf("raw disconnect: %v", err)
		}
	})
}

func TestMalformedConnect(t *testing.T) {
	for _, packet := range [][]byte{
		{0xff, 1},
		{0xff, 1, 6, 8, 8, 0, 1, 1},  // reserved granularity
		{0xff, 1, 0, 7, 8, 0, 1, 1},  // too small CTO
		{0xff, 1, 0, 12, 8, 0, 1, 1}, // CTO exceeds classical CAN
	} {
		t.Run(fmt.Sprintf("%x", packet), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, false)
				done := run(func() error { _, err := r.client.Connect(r.ctx); return err })
				r.expect(0xff, 0)
				r.reply(packet...)
				if err := <-done; !errors.Is(err, xcp.ErrInvalidResponse) {
					t.Fatalf("CONNECT: %v", err)
				}
				if _, err := r.client.Read(r.ctx, xcp.Address{}, 1); !errors.Is(err, xcp.ErrSynchronizationRequired) {
					t.Fatalf("malformed CONNECT trusted: %v", err)
				}
			})
		})
	}
}

func TestFDRoundingAndSessionTermination(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, true)
		r.client.Close()
		r.config.PadFrames = false
		var err error
		r.client, err = xcp.New(r.tester, r.config)
		if err != nil {
			t.Fatal(err)
		}
		defer r.client.Close()
		done := run(func() error { _, err := r.client.Connect(r.ctx); return err })
		r.expect(0xff, 0) // Bootstrap is FD even though CONNECT needs only two bytes.
		r.reply(0xff, 1, 0, 64, 64, 0, 1, 1)
		r.finish(done)
		done = run(func() error {
			_, err := r.client.Do(r.ctx, xcp.Request{Command: 0xf1, Data: []byte{1, 2, 3, 4, 5, 6, 7, 8}})
			return err
		})
		// Nine protocol bytes must round to a 12-byte FD frame, using configured fill.
		r.expect(0xf1, 1, 2, 3, 4, 5, 6, 7, 8, 0xaa, 0xaa, 0xaa)
		r.reply(0xff)
		r.finish(done)
		r.connect()
		done = run(func() error { _, err := r.client.Status(r.ctx); return err })
		r.expect(0xfd)
		r.reply(0xfd, 7)
		if err := <-done; !errors.Is(err, xcp.ErrSessionTerminated) {
			t.Fatalf("termination: %v", err)
		}
		if _, err := r.client.Status(r.ctx); !errors.Is(err, xcp.ErrNotConnected) {
			t.Fatalf("terminated session: %v", err)
		}
		// A disconnected ECU ignores SYNCH. Explicit termination must permit CONNECT.
		r.connect()
	})
}

func TestConnectRetryBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, false)
		done := run(func() error { _, err := r.client.Connect(r.ctx); return err })
		r.expect(0xff, 0)
		time.Sleep(21 * time.Millisecond)
		if err := <-done; !errors.Is(err, xcp.ErrTimeout) {
			t.Fatal(err)
		}
		if err := r.client.Synchronize(r.ctx); !errors.Is(err, xcp.ErrNotConnected) {
			t.Fatalf("SYNCH after unanswered CONNECT: %v", err)
		}
		// The ECU might still be disconnected. Allow another CONNECT, not a
		// prerequisite SYNCH which disconnected ECUs need not answer.
		done = run(func() error { _, err := r.client.Connect(r.ctx); return err })
		r.expect(0xff, 0)
		r.reply(0xfe, 0x33)
		var negative *xcp.NegativeResponseError
		if err := <-done; !errors.As(err, &negative) {
			t.Fatal(err)
		}
		if _, err := r.client.Status(r.ctx); !errors.Is(err, xcp.ErrSynchronizationRequired) {
			t.Fatalf("negative retry lost uncertainty: %v", err)
		}
		done = run(func() error { _, err := r.client.Connect(r.ctx); return err })
		r.expect(0xff, 0)
		r.reply(0xff, 1, 0, 8, 8, 0, 1, 1) // could be an earlier reply
		r.expect(0xfc)
		time.Sleep(21 * time.Millisecond)
		if err := <-done; !errors.Is(err, xcp.ErrTimeout) {
			t.Fatal(err)
		}
		// An old SYNCH marker cannot fence a CONNECT sent after it. No newer
		// CONNECT may pass until explicit synchronisation has succeeded.
		if _, err := r.client.Connect(r.ctx); !errors.Is(err, xcp.ErrSynchronizationRequired) {
			t.Fatalf("CONNECT passed failed barrier: %v", err)
		}
		r.reply(0xfe, 0) // Resume the private CONNECT barrier without another SYNCH.
		if err := r.client.Synchronize(r.ctx); err != nil {
			t.Fatal(err)
		}
		r.connect()
		// A second timeout/retry succeeds, but only fresh post-barrier capabilities
		// are exposed. Older CONNECT replies and unrelated errors are drained.
		done = run(func() error { _, err := r.client.Connect(r.ctx); return err })
		r.expect(0xff, 0)
		time.Sleep(21 * time.Millisecond)
		if err := <-done; !errors.Is(err, xcp.ErrTimeout) {
			t.Fatal(err)
		}
		done = run(func() error {
			caps, err := r.client.Connect(r.ctx)
			if err == nil && caps.Resources != 5 {
				return fmt.Errorf("stale capabilities: %+v", caps)
			}
			return err
		})
		r.expect(0xff, 0)
		r.reply(0xff, 1, 0, 8, 8, 0, 1, 1)
		r.expect(0xfc)
		r.reply(0xff, 1, 0, 8, 8, 0, 1, 1)
		r.reply(0xfe, 0x33)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("accepted unfenced CONNECT: %v", err)
		default:
		}
		r.reply(0xfe, 0)
		r.expect(0xff, 0)
		r.reply(0xff, 5, 0, 8, 8, 0, 1, 1)
		r.finish(done)
		done = run(func() error { _, err := r.client.Status(r.ctx); return err })
		r.expect(0xfd)
		r.reply(0xff, 0, 0, 0, 0, 0)
		r.finish(done)
	})
}

func TestDisconnectedRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, false)
		if err := r.client.Synchronize(r.ctx); !errors.Is(err, xcp.ErrNotConnected) {
			t.Fatalf("fresh SYNCH: %v", err)
		}
		if err := r.client.Disconnect(r.ctx); !errors.Is(err, xcp.ErrNotConnected) {
			t.Fatalf("fresh DISCONNECT: %v", err)
		}
		r.connect() // This must be the first frame; the rejected calls send nothing.
		done := run(func() error { return r.client.Disconnect(r.ctx) })
		r.expect(0xfe)
		r.reply(0xff)
		r.finish(done)
		if _, err := r.client.Do(r.ctx, xcp.Request{Command: xcp.CommandSynch}); !errors.Is(err, xcp.ErrNotConnected) {
			t.Fatalf("raw SYNCH while disconnected: %v", err)
		}
		r.connect()
		done = run(func() error { return r.client.Disconnect(r.ctx) })
		r.expect(0xfe)
		time.Sleep(21 * time.Millisecond)
		if err := <-done; !errors.Is(err, xcp.ErrTimeout) {
			t.Fatal(err)
		}
		if err := r.client.Synchronize(r.ctx); !errors.Is(err, xcp.ErrNotConnected) {
			t.Fatalf("SYNCH after unanswered DISCONNECT: %v", err)
		}
		done = run(func() error { _, err := r.client.Connect(r.ctx); return err })
		r.expect(0xff, 0)
		r.reply(0xff, 0, 0, 0, 0, 0, 0, 0) // Delayed padded DISCONNECT acknowledgement.
		if err := <-done; !errors.Is(err, xcp.ErrInvalidResponse) {
			t.Fatalf("invalid CONNECT candidate: %v", err)
		}
		// A candidate must decode as CONNECT before it justifies sending SYNCH.
		done = run(func() error {
			caps, err := r.client.Connect(r.ctx)
			if err == nil && caps.Resources != 5 {
				return fmt.Errorf("stale reconnect capabilities: %+v", caps)
			}
			return err
		})
		r.expect(0xff, 0)
		r.reply(0xff, 1, 0, 8, 8, 0, 1, 1)
		r.expect(0xfc)
		r.reply(0xff, 1, 0, 8, 8, 0, 1, 1) // Response to the first reconnect, still pre-barrier.
		r.reply(0xfe, 0)
		r.expect(0xff, 0)
		r.reply(0xff, 5, 0, 8, 8, 0, 1, 1)
		r.finish(done)
		done = run(func() error {
			data, err := r.client.ShortUpload(r.ctx, xcp.Address{Value: 0x1000}, 1)
			if err == nil && !bytes.Equal(data, []byte{0x42}) {
				return fmt.Errorf("reconnected read: %x", data)
			}
			return err
		})
		r.expect(0xf4, 1, 0, 0, 0, 0x10, 0, 0)
		r.reply(0xff, 0x42)
		r.finish(done)
	})
}

func TestDefiniteSendOutcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, false)
		r.client.Close()
		bus := &outcomeBus{Bus: r.tester}
		var err error
		r.client, err = xcp.New(bus, r.config)
		if err != nil {
			t.Fatal(err)
		}
		defer r.client.Close()
		r.connect()
		ctx, cancel := context.WithCancel(r.ctx)
		bus.reject = gocan.ErrTransmitQueueFull
		bus.after = cancel
		if _, err := r.client.Status(ctx); !errors.Is(err, gocan.ErrTransmitQueueFull) {
			t.Fatalf("definite rejection masked: %v", err)
		}
		// No command was accepted, so no SYNCH is needed after a rejected send.
		done := run(func() error { _, err := r.client.Status(r.ctx); return err })
		r.expect(0xfd)
		r.reply(0xff, 0, 0, 0, 0, 0)
		r.finish(done)
		ctx, cancel = context.WithCancel(r.ctx)
		bus.after = cancel
		done = run(func() error { _, err := r.client.Status(ctx); return err })
		r.expect(0xfd) // Cancellation coincides with a definite accepted native send.
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("accepted cancellation: %v", err)
		}
		if _, err := r.client.Status(r.ctx); !errors.Is(err, xcp.ErrSynchronizationRequired) {
			t.Fatalf("accepted send not tracked: %v", err)
		}
		r.synchronize()
	})
}

// Model the Bus contract's definite native result, including cancellation while
// the native API is returning it. It delegates all accepted traffic to virtual.
type outcomeBus struct {
	gocan.Bus
	reject error
	after  func()
}

func (bus *outcomeBus) Send(ctx context.Context, frame gocan.Frame) error {
	err, after := bus.reject, bus.after
	bus.reject, bus.after = nil, nil
	if err == nil {
		err = bus.Bus.Send(ctx, frame)
	}
	if after != nil {
		after()
	}
	return err
}

type rig struct {
	t           *testing.T
	ctx         context.Context
	client      *xcp.Client
	capture     *gocan.Capture
	tester, ecu gocan.Bus
	config      xcp.Config
	cursor      gocan.Cursor
}

func newRig(t *testing.T, fd bool) *rig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	capture := gocan.NewCapture()
	var network virtual.Network
	tester, err := network.Open(ctx, capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	ecu, err := network.Open(ctx, capture, virtual.Config{ID: 2, Name: "ECU"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tester.Close(); _ = ecu.Close() })
	config := xcp.Config{TransmitID: 0x600, ReceiveID: 0x601, Timeout: 20 * time.Millisecond}
	if fd {
		config.TransmitID = 0x123456
		config.ReceiveID = 0x123457
		config.FrameFlags = gocan.FrameExtended | gocan.FrameFD | gocan.FrameBitRateSwitch
		config.TransmitDataLength = 64
		config.PadFrames = true
		config.PaddingByte = 0xaa
	}
	client, err := xcp.New(tester, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return &rig{t: t, ctx: ctx, client: client, capture: capture, tester: tester, ecu: ecu, config: config}
}

func run(call func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- call() }()
	return done
}
func (r *rig) finish(done <-chan error) {
	r.t.Helper()
	if err := <-done; err != nil {
		r.t.Fatal(err)
	}
}
func (r *rig) receiveKey() gocan.FrameKey {
	return gocan.FrameKey{Bus: r.tester.ID(), ID: r.config.ReceiveID, Direction: gocan.DirectionReceive, Extended: r.config.FrameFlags.Has(gocan.FrameExtended)}
}

func (r *rig) expect(want ...byte) {
	r.t.Helper()
	key := gocan.FrameKey{Bus: r.ecu.ID(), ID: r.config.TransmitID, Direction: gocan.DirectionReceive, Extended: r.config.FrameFlags.Has(gocan.FrameExtended)}
	event, cursor, err := r.capture.Next(r.ctx, key, r.cursor)
	if err != nil {
		r.t.Fatal(err)
	}
	r.cursor = cursor
	if r.config.PadFrames {
		for len(want) < 64 {
			want = append(want, 0xaa)
		}
	}
	if event.Frame.Flags != r.config.FrameFlags || !bytes.Equal(event.Frame.Data[:event.Frame.DataLength()], want) {
		r.t.Fatalf("request %x flags %x, want %x flags %x", event.Frame.Data[:event.Frame.DataLength()], event.Frame.Flags, want, r.config.FrameFlags)
	}
}

func (r *rig) reply(data ...byte) {
	r.t.Helper()
	if r.config.PadFrames {
		for len(data) < 64 {
			data = append(data, 0xaa)
		}
	}
	frame, err := gocan.NewFrame(r.config.ReceiveID, data, r.config.FrameFlags)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.ecu.Send(r.ctx, frame); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) connect() {
	r.t.Helper()
	done := run(func() error { _, err := r.client.Connect(r.ctx); return err })
	r.expect(0xff, 0)
	r.reply(0xff, 1, 0, 8, 8, 0, 1, 1)
	r.finish(done)
}

func (r *rig) synchronize() {
	r.t.Helper()
	done := run(func() error { return r.client.Synchronize(r.ctx) })
	r.expect(0xfc)
	r.reply(0xfe, 0)
	r.finish(done)
}
