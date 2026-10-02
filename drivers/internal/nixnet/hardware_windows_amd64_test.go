//go:build windows && amd64

package nixnet

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

func TestInventoryHardware(t *testing.T) {
	if os.Getenv("GOCAN_NIXNET_CHANNEL_A") == "" {
		t.Skip("NI-XNET hardware not selected")
	}
	channels, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range channels {
		t.Logf("%+v", c)
	}
}

func TestQueueContractsHardware(t *testing.T) {
	aName, bName := os.Getenv("GOCAN_NIXNET_CHANNEL_A"), os.Getenv("GOCAN_NIXNET_CHANNEL_B")
	if aName == "" || bName == "" {
		t.Skip("GOCAN_NIXNET_CHANNEL_A/B not set")
	}
	if aName == bName {
		t.Fatal("distinct interfaces required")
	}
	capture := gocan.NewCapture()
	a, err := Open(context.Background(), capture, Config{ID: 1, Name: aName, Interface: aName, Baud: 500000, Termination: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	b, err := Open(context.Background(), capture, Config{ID: 2, Name: bName, Interface: bName, Baud: 500000, Termination: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	rxBytes, err := b.api.property(b.rx, propQueueSize)
	if err != nil || len(rxBytes) != 4 {
		t.Fatalf("read RX queue: %v", err)
	}
	txBytes, err := a.api.property(a.tx, propQueueSize)
	if err != nil || len(txBytes) != 4 {
		t.Fatalf("read TX queue: %v", err)
	}
	rxSize, txSize := binary.LittleEndian.Uint32(rxBytes), binary.LittleEndian.Uint32(txBytes)
	t.Logf("native queue bytes: RX=%d TX=%d", rxSize, txSize)
	count := int(rxSize)/24*2 + 2048
	if count > 50000 {
		t.Skipf("RX overflow requires more than test budget of 50000 frames: %d", count)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	frame, _ := gocan.NewFrame(0x321, make([]byte, 8), 0)
	// Stalling host reads does not stop the peer's controller acknowledging.
	// This exercises the real native overflow status and terminal capture event.
	b.ioMu.Lock()
	defer b.ioMu.Unlock()
	rejected := 0
	sent := 0
	for sent < count {
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		err := a.Send(ctx, frame)
		if errors.Is(err, gocan.ErrTransmitQueueFull) {
			rejected++
			if got := len(capture.Series(gocan.FrameKey{Bus: 1, ID: frame.ID, Direction: gocan.DirectionTransmit})); got != sent {
				t.Fatalf("rejected TX was recorded: got %d want %d", got, sent)
			}
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		sent++
	}
	t.Logf("accepted=%d rejected=%d", sent, rejected)
	if rejected == 0 {
		t.Fatal("did not exercise native TX backpressure")
	}
	b.ioMu.Unlock()
	select {
	case <-b.Done():
	case <-ctx.Done():
		b.ioMu.Lock()
		t.Fatal("RX overflow did not stop the bus")
	}
	b.ioMu.Lock()
	foundOverrun := false
	for _, event := range capture.BusEvents(b.ID()) {
		if event.Kind == gocan.EventReceiveOverrun {
			foundOverrun = true
		}
	}
	if !foundOverrun {
		t.Fatal("native RX overrun did not produce a capture event")
	}
	if !errors.Is(b.Err(), gocan.ErrReceiveOverrun) {
		t.Fatalf("RX failure %v, want overrun", b.Err())
	}
	if err := a.Send(ctx, frame); err != nil {
		t.Fatalf("healthy peer after overrun: %v", err)
	}
}
