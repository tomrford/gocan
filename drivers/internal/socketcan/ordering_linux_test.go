//go:build linux

package socketcan

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

func TestVCanReplyDuringSend(t *testing.T) {
	iface := os.Getenv("GOCAN_VCAN_INTERFACE")
	if iface == "" {
		t.Skip("GOCAN_VCAN_INTERFACE is not set")
	}
	capture := gocan.NewCapture()
	closeBus := func(bus *Bus) {
		t.Helper()
		closed := make(chan struct{})
		go func() { _ = bus.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatal("Close did not wake acquisition")
		}
	}
	open := func(id gocan.BusID) *Bus {
		bus, err := Open(context.Background(), capture, Config{ID: id, Interface: iface})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeBus(bus) })
		return bus
	}
	target, peer := open(1), open(2)
	request, err := gocan.NewFrame(0x123, []byte{1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := gocan.NewFrame(0x456, []byte{2}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	replied := make(chan error, 1)
	go func() {
		_, _, err := capture.Next(ctx, gocan.FrameKey{
			Bus: peer.ID(), ID: request.ID, Direction: gocan.DirectionReceive,
		}, gocan.Cursor{})
		if err == nil {
			err = peer.Send(ctx, reply)
		}
		replied <- err
	}()
	replyKey := gocan.FrameKey{Bus: target.ID(), ID: reply.ID, Direction: gocan.DirectionReceive}
	var peerErr error
	target.sendReady = func(fd uintptr) {
		target.sendOnce(fd)
		// Keep the real native send callback open until the peer has replied,
		// then give RX a chance to record it before Send records its TX.
		peerErr = <-replied
		wait, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, _, _ = capture.Next(wait, replyKey, gocan.Cursor{})
	}
	if err := target.Send(ctx, request); err != nil {
		t.Fatal(err)
	}
	if peerErr != nil {
		t.Fatalf("peer reply: %v", peerErr)
	}
	if _, _, err := capture.Next(ctx, replyKey, gocan.Cursor{}); err != nil {
		t.Fatalf("receive reply: %v", err)
	}
	var frames []gocan.FrameEvent
	for _, event := range capture.Frames() {
		if event.Bus == target.ID() {
			frames = append(frames, event)
		}
	}
	if len(frames) != 2 || frames[0].Direction != gocan.DirectionTransmit || frames[1].Direction != gocan.DirectionReceive {
		t.Fatalf("want request TX before reply RX, got %+v", frames)
	}
	if frames[1].Timestamp.Before(frames[0].Timestamp) {
		t.Fatal("reply timestamp precedes request timestamp")
	}
	// Close after the reply leaves acquisition waiting on an idle socket.
	closeBus(target)
	if err := target.Send(ctx, request); !errors.Is(err, gocan.ErrBusClosed) {
		t.Fatalf("Send after Close = %v", err)
	}
}
