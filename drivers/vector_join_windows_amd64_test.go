//go:build windows && amd64

package drivers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/asc"
)

const vectorJoinTimeout = 5 * time.Second

// TestVectorJoinLifecycle joins a Vector channel that another port already
// configured, as BOOST joins a channel CANoe owns during a measurement. XL
// grants initialization access per port, so a second port in this process
// stands in for the other application. A PCAN adapter on the same bus is the
// independent peer. The joined bus's capture is written as an ASC trace.
func TestVectorJoinLifecycle(t *testing.T) {
	vectorValue := os.Getenv("GOCAN_VECTOR_CHANNEL_INDEX")
	pcanValue := os.Getenv("GOCAN_PCAN_CHANNEL")
	if vectorValue == "" || pcanValue == "" {
		t.Skip("GOCAN_VECTOR_CHANNEL_INDEX and GOCAN_PCAN_CHANNEL are not set")
	}
	vectorIndex, err := strconv.ParseUint(vectorValue, 0, 8)
	if err != nil || vectorIndex >= 64 {
		t.Fatalf("GOCAN_VECTOR_CHANNEL_INDEX=%q is not a channel index from 0 through 63", vectorValue)
	}
	pcanHandle, err := strconv.ParseUint(pcanValue, 0, 16)
	if err != nil || pcanHandle == 0 {
		t.Fatalf("GOCAN_PCAN_CHANNEL=%q is not a nonzero 16-bit PCAN handle", pcanValue)
	}
	vectorID := fmt.Sprintf("vector:%d", vectorIndex)
	pcanID := fmt.Sprintf("pcan:%#x", pcanHandle)
	ctx := context.Background()

	// Stage 1: an idle channel is programmable.
	idle := discoveredChannel(t, vectorID)
	if idle.ExternallyConfigured() {
		t.Fatalf("%s is in use before the test; close other applications using it", vectorID)
	}
	if bitrate, dataBitrate := idle.ExternalBitrates(); bitrate != 0 || dataBitrate != 0 {
		t.Fatalf("idle %s reports external rates %d/%d", vectorID, bitrate, dataBitrate)
	}

	// Stage 2: another port configures the channel at 500 kbit/s.
	capture := gocan.NewCapture()
	owner, err := Open(ctx, capture, idle, Config{ID: 1, Name: "owner", Bitrate: 500_000})
	if err != nil {
		t.Fatalf("open owner: %v", err)
	}
	ownerOpen := true
	t.Cleanup(func() {
		if ownerOpen {
			_ = owner.Close()
		}
	})

	shared := discoveredChannel(t, vectorID)
	if !shared.ExternallyConfigured() {
		t.Fatalf("%s is not reported as externally configured while another port owns it", vectorID)
	}
	if bitrate, dataBitrate := shared.ExternalBitrates(); bitrate != 500_000 || dataBitrate != 0 {
		t.Fatalf("shared %s reports rates %d/%d, want 500000/0", vectorID, bitrate, dataBitrate)
	}
	if len(shared.FDRatePresets()) != 0 {
		t.Fatalf("shared %s offers FD presets %+v", vectorID, shared.FDRatePresets())
	}
	if bus, err := Open(ctx, gocan.NewCapture(), idle, Config{ID: 9, Name: "second-owner", Bitrate: 250_000}); err == nil {
		_ = bus.Close()
		t.Fatal("configured a channel that another port owns")
	}
	requireOpen(t, owner)

	// Stage 3: join, and exchange frames with an independent peer.
	joined, err := Open(ctx, capture, shared, Config{ID: 2, Name: "joined", External: true})
	if err != nil {
		t.Fatalf("join %s: %v", vectorID, err)
	}
	t.Cleanup(func() { _ = joined.Close() })
	peer, err := Open(ctx, capture, discoveredChannel(t, pcanID), Config{ID: 3, Name: "pcan-peer", Bitrate: 500_000})
	if err != nil {
		t.Fatalf("open PCAN peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	requireExchange(t, capture, peer, joined, 0x5a1)
	requireExchange(t, capture, joined, peer, 0x5a2)

	// Stage 4: the owner's transmissions do not stop the joined bus.
	ownerFrame, err := gocan.NewFrame(0x5a3, []byte{0xa5}, 0)
	if err != nil {
		t.Fatalf("build owner frame: %v", err)
	}
	if err := owner.Send(ctx, ownerFrame); err != nil {
		t.Fatalf("send from owner: %v", err)
	}
	waitForFrame(t, capture, peer, ownerFrame.ID)
	requireExchange(t, capture, peer, joined, 0x5a4)
	requireOpen(t, joined)

	// Stage 5: the flashing port closes without interrupting the test application.
	if err := joined.Close(); err != nil {
		t.Fatalf("close joined bus while owner is active: %v", err)
	}
	requireExchange(t, capture, peer, owner, 0x5a7)
	requireExchange(t, capture, owner, peer, 0x5a8)
	rejoined, err := Open(ctx, capture, shared, Config{ID: 2, Name: "rejoined", External: true})
	if err != nil {
		t.Fatalf("rejoin %s: %v", vectorID, err)
	}
	t.Cleanup(func() { _ = rejoined.Close() })

	// Stage 6: the owner closes; the joined bus keeps the channel on the bus.
	ownerOpen = false
	if err := owner.Close(); err != nil {
		t.Fatalf("close owner: %v", err)
	}
	requireExchange(t, capture, peer, rejoined, 0x5a5)
	requireExchange(t, capture, rejoined, peer, 0x5a6)
	if !discoveredChannel(t, vectorID).ExternallyConfigured() {
		t.Fatal("channel left the bus while the joined port still had it active")
	}

	// Stage 7: closing the last port takes the channel off the bus. A real
	// discovery snapshot from before it closed must no longer allow a join.
	if err := rejoined.Close(); err != nil {
		t.Fatalf("close joined bus: %v", err)
	}
	if discoveredChannel(t, vectorID).ExternallyConfigured() {
		t.Fatal("channel stayed on the bus after its last port closed")
	}
	if bus, err := Open(ctx, gocan.NewCapture(), shared, Config{ID: 9, Name: "stale-join", External: true}); err == nil {
		_ = bus.Close()
		t.Fatal("joined a channel that no application had on the bus")
	}
	if discoveredChannel(t, vectorID).ExternallyConfigured() {
		t.Fatal("a refused join left the channel on the bus")
	}

	writeJoinTrace(t, capture)
}

func discoveredChannel(t *testing.T, identifier string) Channel {
	t.Helper()
	channels, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, channel := range channels {
		if channel.Identifier() == identifier {
			return channel
		}
	}
	t.Fatalf("%s is absent from discovery result %+v", identifier, channels)
	return Channel{}
}

func requireExchange(t *testing.T, capture *gocan.Capture, from, to gocan.Bus, identifier uint32) {
	t.Helper()
	frame, err := gocan.NewFrame(identifier, []byte{byte(identifier), 0x5a}, 0)
	if err != nil {
		t.Fatalf("build frame %#x: %v", identifier, err)
	}
	cursor := capture.End()
	if err := from.Send(context.Background(), frame); err != nil {
		t.Fatalf("send %#x from %s: %v", identifier, from.Name(), err)
	}
	event := waitForFrameSince(t, capture, to, identifier, cursor)
	if event.Frame != frame {
		t.Fatalf("%s received %+v, want %+v", to.Name(), event.Frame, frame)
	}
}

func waitForFrame(t *testing.T, capture *gocan.Capture, on gocan.Bus, identifier uint32) gocan.FrameEvent {
	t.Helper()
	return waitForFrameSince(t, capture, on, identifier, gocan.Cursor{})
}

func waitForFrameSince(t *testing.T, capture *gocan.Capture, on gocan.Bus, identifier uint32, cursor gocan.Cursor) gocan.FrameEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), vectorJoinTimeout)
	defer cancel()
	event, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       on.ID(),
		ID:        identifier,
		Direction: gocan.DirectionReceive,
	}, cursor)
	if err != nil {
		t.Fatalf("wait for %#x on %s: %v (bus error: %v)", identifier, on.Name(), err, on.Err())
	}
	return event
}

func requireOpen(t *testing.T, bus gocan.Bus) {
	t.Helper()
	select {
	case <-bus.Done():
		t.Fatalf("%s stopped: %v", bus.Name(), bus.Err())
	default:
	}
}

func writeJoinTrace(t *testing.T, capture *gocan.Capture) {
	t.Helper()
	path := filepath.Join(os.TempDir(), "gocan-vector-join.asc")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create join trace: %v", err)
	}
	writer := asc.NewWriter(file)
	if _, err := capture.WriteRecordsSince(gocan.Cursor{}, writer); err != nil {
		_ = file.Close()
		t.Fatalf("write join trace: %v", err)
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		t.Fatalf("flush join trace: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close join trace: %v", err)
	}
	t.Logf("joined bus trace: %s", path)
}
