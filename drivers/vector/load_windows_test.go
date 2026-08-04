//go:build windows && amd64

package vector

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/pcan"
)

const hardwareLoadTimeout = 30 * time.Second

func TestVectorTransmitQueueFull(t *testing.T) {
	vectorIndex := vectorChannelIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX")
	pcanChannel := pcanPeerChannel(t, "GOCAN_PCAN_CHANNEL")

	frame, err := gocan.NewFrame(0x590, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	runVectorTransmitQueueFull(t, vectorSaturationSetup{
		target: Config{
			ID: 2, Name: "vector", ChannelIndex: vectorIndex, Bitrate: 500_000,
		},
		openPeer: func(capture *gocan.Capture) (gocan.Bus, error) {
			return pcan.Open(context.Background(), capture, pcan.Config{
				ID: 1, Name: "pcan-peer", Channel: pcanChannel, Bitrate: pcan.Bitrate500K,
			})
		},
		frame: frame,
	})
}

func TestVectorFDTransmitQueueFull(t *testing.T) {
	vectorA, vectorB := vectorPairIndexes(t)
	dataBitrate := vectorFDDataBitrate(t)
	frame, err := gocan.NewFrame(
		0x594,
		make([]byte, 64),
		gocan.FrameFD|gocan.FrameBitRateSwitch,
	)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	runVectorTransmitQueueFull(t, vectorSaturationSetup{
		target: Config{
			ID: 2, Name: "vector-fd", ChannelIndex: vectorA,
			Bitrate: 500_000, DataBitrate: dataBitrate,
		},
		openPeer: func(capture *gocan.Capture) (gocan.Bus, error) {
			return Open(context.Background(), capture, Config{
				ID: 1, Name: "vector-fd-peer", ChannelIndex: vectorB,
				Bitrate: 500_000, DataBitrate: dataBitrate,
			})
		},
		frame: frame,
	})
}

type vectorSaturationSetup struct {
	target   Config
	openPeer func(*gocan.Capture) (gocan.Bus, error)
	frame    gocan.Frame
}

func runVectorTransmitQueueFull(t *testing.T, setup vectorSaturationSetup) {
	t.Helper()
	capture := gocan.NewCapture()
	peer, err := setup.openPeer(capture)
	if err != nil {
		t.Fatalf("open saturation peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	vectorBus, err := Open(context.Background(), capture, setup.target)
	if err != nil {
		t.Fatalf("open Vector target: %v", err)
	}
	t.Cleanup(func() { _ = vectorBus.Close() })

	accepted := 0
	for range 10_000 {
		err := vectorBus.Send(context.Background(), setup.frame)
		if err == nil {
			accepted++
			continue
		}
		if !errors.Is(err, gocan.ErrTransmitQueueFull) {
			t.Fatalf("saturated Vector Send = %v, want ErrTransmitQueueFull", err)
		}
		break
	}
	if accepted == 10_000 {
		t.Fatal("Vector transmit queue did not fill")
	}
	key := gocan.FrameKey{
		Bus:       vectorBus.ID(),
		ID:        setup.frame.ID,
		Direction: gocan.DirectionTransmit,
	}
	if got := len(capture.Series(key)); got != accepted {
		t.Fatalf("capture holds %d transmissions after %d accepted sends and one rejection", got, accepted)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		err := vectorBus.Send(context.Background(), setup.frame)
		if err == nil {
			break
		}
		if !errors.Is(err, gocan.ErrTransmitQueueFull) {
			t.Fatalf("Vector Send while queue drains = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("Vector transmit queue did not recover")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestVectorThreeAdapterLoad(t *testing.T) {
	pcanChannel := pcanPeerChannel(t, "GOCAN_PCAN_CHANNEL")
	vectorAIndex, vectorBIndex := vectorPairIndexes(t)
	framesPerAdapter, sendInterval := loadTestParameters(t)

	capture := gocan.NewCapture()
	pcanBus, err := pcan.Open(context.Background(), capture, pcan.Config{
		ID:      1,
		Name:    "pcan",
		Channel: pcanChannel,
		Bitrate: pcan.Bitrate500K,
	})
	if err != nil {
		t.Fatalf("open PCAN: %v", err)
	}
	t.Cleanup(func() { _ = pcanBus.Close() })

	vectorA, err := Open(context.Background(), capture, Config{
		ID:           2,
		Name:         "vector-a",
		ChannelIndex: vectorAIndex,
		Bitrate:      500_000,
	})
	if err != nil {
		t.Fatalf("open first Vector channel: %v", err)
	}
	t.Cleanup(func() { _ = vectorA.Close() })

	vectorB, err := Open(context.Background(), capture, Config{
		ID:           3,
		Name:         "vector-b",
		ChannelIndex: vectorBIndex,
		Bitrate:      500_000,
	})
	if err != nil {
		t.Fatalf("open second Vector channel: %v", err)
	}
	t.Cleanup(func() { _ = vectorB.Close() })

	buses := []gocan.Bus{pcanBus, vectorA, vectorB}
	started := time.Now()
	sendThreeAdapterLoad(t, buses, framesPerAdapter, sendInterval)
	verifyThreeAdapterLoad(t, capture, buses, framesPerAdapter)
	elapsed := time.Since(started)
	t.Logf(
		"delivered %d frames from three adapters in %s (%.0f frames/s on the shared medium)",
		3*framesPerAdapter,
		elapsed.Round(time.Millisecond),
		float64(3*framesPerAdapter)/elapsed.Seconds(),
	)
	if sendInterval != 0 {
		t.Logf("paced each adapter at one submission every %s", sendInterval)
	}

	if err := vectorA.Close(); err != nil {
		t.Fatalf("close first Vector channel: %v", err)
	}
	assertPeerTraffic(t, capture, pcanBus, vectorB, 0x580)
	assertPeerTraffic(t, capture, vectorB, pcanBus, 0x581)
}

func TestTwoPCANVectorLoad(t *testing.T) {
	pcanAChannel := pcanPeerChannel(t, "GOCAN_PCAN_CHANNEL_A")
	pcanBChannel := pcanPeerChannel(t, "GOCAN_PCAN_CHANNEL_B")
	vectorIndex := vectorChannelIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX")
	if pcanAChannel == pcanBChannel {
		t.Fatalf("GOCAN_PCAN_CHANNEL_A and GOCAN_PCAN_CHANNEL_B both select channel %#x", uint16(pcanAChannel))
	}
	framesPerAdapter, sendInterval := loadTestParameters(t)

	capture := gocan.NewCapture()
	pcanA, err := pcan.Open(context.Background(), capture, pcan.Config{
		ID: 1, Name: "pcan-a", Channel: pcanAChannel, Bitrate: pcan.Bitrate500K,
	})
	if err != nil {
		t.Fatalf("open first PCAN: %v", err)
	}
	t.Cleanup(func() { _ = pcanA.Close() })
	pcanB, err := pcan.Open(context.Background(), capture, pcan.Config{
		ID: 2, Name: "pcan-b", Channel: pcanBChannel, Bitrate: pcan.Bitrate500K,
	})
	if err != nil {
		t.Fatalf("open second PCAN: %v", err)
	}
	t.Cleanup(func() { _ = pcanB.Close() })
	vectorBus, err := Open(context.Background(), capture, Config{
		ID: 3, Name: "vector", ChannelIndex: vectorIndex, Bitrate: 500_000,
	})
	if err != nil {
		t.Fatalf("open Vector: %v", err)
	}
	t.Cleanup(func() { _ = vectorBus.Close() })

	buses := []gocan.Bus{pcanA, pcanB, vectorBus}
	started := time.Now()
	sendThreeAdapterLoad(t, buses, framesPerAdapter, sendInterval)
	verifyThreeAdapterLoad(t, capture, buses, framesPerAdapter)
	t.Logf(
		"delivered %d frames from two PCAN adapters and Vector in %s (%.0f frames/s on the shared medium)",
		3*framesPerAdapter,
		time.Since(started).Round(time.Millisecond),
		float64(3*framesPerAdapter)/time.Since(started).Seconds(),
	)

	if err := pcanA.Close(); err != nil {
		t.Fatalf("close first PCAN channel: %v", err)
	}
	assertPeerTraffic(t, capture, pcanB, vectorBus, 0x582)
	assertPeerTraffic(t, capture, vectorBus, pcanB, 0x583)
}

func loadTestParameters(t *testing.T) (int, time.Duration) {
	t.Helper()
	framesPerAdapter := uint64(250)
	if value := os.Getenv("GOCAN_LOAD_FRAMES"); value != "" {
		framesPerAdapter = parseHardwareUint(t, "GOCAN_LOAD_FRAMES", value, 31)
		if framesPerAdapter == 0 {
			t.Fatal("GOCAN_LOAD_FRAMES must be nonzero")
		}
	}
	var sendInterval time.Duration
	if value := os.Getenv("GOCAN_LOAD_INTERVAL"); value != "" {
		parsedInterval, parseErr := time.ParseDuration(value)
		if parseErr != nil || parsedInterval < 0 {
			t.Fatalf("GOCAN_LOAD_INTERVAL=%q is not a non-negative duration", value)
		}
		sendInterval = parsedInterval
	}
	return int(framesPerAdapter), sendInterval
}

func sendThreeAdapterLoad(t *testing.T, buses []gocan.Bus, framesPerAdapter int, sendInterval time.Duration) {
	t.Helper()
	start := make(chan struct{})
	errors := make(chan error, len(buses))
	var senders sync.WaitGroup
	for source, bus := range buses {
		senders.Add(1)
		go func() {
			defer senders.Done()
			<-start
			var pace *time.Ticker
			if sendInterval != 0 {
				pace = time.NewTicker(sendInterval)
				defer pace.Stop()
			}
			for sequence := range framesPerAdapter {
				if pace != nil && sequence != 0 {
					<-pace.C
				}
				var data [8]byte
				binary.LittleEndian.PutUint32(data[:4], uint32(sequence))
				data[4] = byte(source)
				frame, err := gocan.NewFrame(uint32(0x500+source), data[:], 0)
				if err != nil {
					errors <- fmt.Errorf("build frame from %s: %w", bus.Name(), err)
					return
				}
				if err := bus.Send(context.Background(), frame); err != nil {
					errors <- fmt.Errorf("send sequence %d from %s: %w", sequence, bus.Name(), err)
					return
				}
			}
		}()
	}
	close(start)
	senders.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
}

func verifyThreeAdapterLoad(t *testing.T, capture *gocan.Capture, buses []gocan.Bus, framesPerAdapter int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hardwareLoadTimeout)
	defer cancel()

	for source, sourceBus := range buses {
		for _, observer := range buses {
			wantDirection := gocan.DirectionReceive
			if observer == sourceBus {
				wantDirection = gocan.DirectionTransmit
			}
			key := gocan.FrameKey{
				Bus:       observer.ID(),
				ID:        uint32(0x500 + source),
				Direction: wantDirection,
			}
			var cursor gocan.Cursor
			for sequence := range framesPerAdapter {
				event, next, err := capture.Next(ctx, key, cursor)
				if err != nil {
					t.Fatalf("wait for sequence %d from %s on %s: %v", sequence, sourceBus.Name(), observer.Name(), err)
				}
				cursor = next
				if event.Direction != wantDirection {
					t.Fatalf("sequence %d from %s on %s has direction %v, want %v", sequence, sourceBus.Name(), observer.Name(), event.Direction, wantDirection)
				}
				if got := binary.LittleEndian.Uint32(event.Frame.Data[:4]); got != uint32(sequence) {
					t.Fatalf("traffic from %s arrived out of order on %s: got %d, want %d", sourceBus.Name(), observer.Name(), got, sequence)
				}
			}
			if got := len(capture.Series(key)); got != framesPerAdapter {
				t.Fatalf("traffic from %s has %d events on %s, want %d", sourceBus.Name(), got, observer.Name(), framesPerAdapter)
			}
		}
	}
}

func assertPeerTraffic(t *testing.T, capture *gocan.Capture, from, to gocan.Bus, identifier uint32) {
	t.Helper()
	frame, err := gocan.NewFrame(identifier, []byte{1, 2, 3}, 0)
	if err != nil {
		t.Fatalf("build peer frame: %v", err)
	}
	cursor := capture.End()
	if err := from.Send(context.Background(), frame); err != nil {
		t.Fatalf("send from %s after peer close: %v", from.Name(), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), hardwareLoadTimeout)
	defer cancel()
	event, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       to.ID(),
		ID:        identifier,
		Direction: gocan.DirectionReceive,
	}, cursor)
	if err != nil {
		t.Fatalf("wait on %s after peer close: %v", to.Name(), err)
	}
	if event.Direction != gocan.DirectionReceive || event.Frame != frame {
		t.Fatalf("traffic on %s after peer close = %+v, want received %+v", to.Name(), event, frame)
	}
}
