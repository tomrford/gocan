package virtual

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/tomrford/gocan"
)

func TestNetworkCapturesAcceptedTransmissionAndReception(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network

	source, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "source"})
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	destination, err := network.Open(context.Background(), capture, Config{ID: 2, Name: "destination"})
	if err != nil {
		t.Fatalf("Open destination: %v", err)
	}
	t.Cleanup(func() { _ = destination.Close() })

	frame, err := gocan.NewFrame(0x123, []byte{1, 2, 3, 4}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	beforeSend := capture.End()
	if err := source.Send(context.Background(), frame); err != nil {
		t.Fatalf("Send: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	transmitted, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       source.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionTransmit,
	}, beforeSend)
	if err != nil {
		t.Fatalf("Next transmitted: %v", err)
	}
	if transmitted.Direction != gocan.DirectionTransmit || transmitted.Frame != frame {
		t.Fatalf("transmitted event = %+v, want source TX frame", transmitted)
	}

	received, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       destination.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionReceive,
	}, beforeSend)
	if err != nil {
		t.Fatalf("Next received: %v", err)
	}
	if received.Direction != gocan.DirectionReceive || received.Frame != frame {
		t.Fatalf("received event = %+v, want destination RX frame", received)
	}
	if received.Timestamp.Before(transmitted.Timestamp) {
		t.Fatalf("RX timestamp %s precedes TX timestamp %s", received.Timestamp, transmitted.Timestamp)
	}

	if series := capture.Series(gocan.FrameKey{
		Bus:       source.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionTransmit,
	}); len(series) != 1 {
		t.Fatalf("source series has %d events, want TX only", len(series))
	}
}

func TestNetworkOpenHonorsCanceledContext(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	bus, err := network.Open(ctx, capture, Config{ID: 1, Name: "canceled"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v, want context.Canceled", err)
	}
	if bus != nil {
		t.Fatal("Open returned a bus for a canceled context")
	}
}

func TestTerminalFailureRejectsLaterSend(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network
	bus, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "terminal"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	bus.fail(gocan.ErrReceiveOverrun)
	frame, err := gocan.NewFrame(0x123, []byte{1}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if err := bus.Send(context.Background(), frame); !errors.Is(err, gocan.ErrReceiveOverrun) {
		t.Fatalf("Send after terminal failure = %v, want ErrReceiveOverrun", err)
	}

	select {
	case <-bus.Done():
	case <-time.After(time.Second):
		t.Fatal("bus did not stop after terminal failure")
	}
	if !errors.Is(bus.Err(), gocan.ErrReceiveOverrun) {
		t.Fatalf("Err after terminal failure = %v, want ErrReceiveOverrun", bus.Err())
	}
	if got := capture.Series(gocan.FrameKey{
		Bus:       bus.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionTransmit,
	}); len(got) != 0 {
		t.Fatalf("captured %d transmissions after terminal failure, want none", len(got))
	}
}

func TestBroadcastDetectsOverrun(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network

	source, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "source"})
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	victim := &Bus{
		id:       2,
		name:     "victim",
		incoming: make(chan gocan.Frame),
		failures: make(chan error, 1),
		done:     make(chan struct{}),
	}
	network.mu.Lock()
	network.buses[victim] = struct{}{}
	network.mu.Unlock()

	network.broadcast(source, gocan.Frame{})
	select {
	case err := <-victim.failures:
		if !errors.Is(err, gocan.ErrReceiveOverrun) {
			t.Fatalf("victim failure = %v, want ErrReceiveOverrun", err)
		}
	default:
		t.Fatal("broadcast did not report an overrun for a full receive queue")
	}
}

func TestBusLifecycleIsIndependentFromSharedCapture(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network

	first, err := network.Open(context.Background(), capture, Config{ID: 1, Name: "first"})
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	second, err := network.Open(context.Background(), capture, Config{
		ID:                 2,
		Name:               "second",
		ReceiveOwnMessages: true,
	})
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if _, err := network.Open(context.Background(), capture, Config{ID: 2, Name: "duplicate"}); err == nil {
		t.Fatal("Open accepted a duplicate bus ID")
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	if first.Err() != nil {
		t.Fatalf("first Err after normal close = %v, want nil", first.Err())
	}
	if err := first.Send(context.Background(), gocan.Frame{}); !errors.Is(err, gocan.ErrBusClosed) {
		t.Fatalf("Send after close error = %v, want ErrBusClosed", err)
	}

	frame, err := gocan.NewFrame(0x321, []byte{9}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	cursor := capture.End()
	if err := second.Send(context.Background(), frame); err != nil {
		t.Fatalf("second Send after first closed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	transmitted, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       second.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionTransmit,
	}, cursor)
	if err != nil {
		t.Fatalf("wait for self transmission: %v", err)
	}
	received, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       second.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionReceive,
	}, cursor)
	if err != nil {
		t.Fatalf("wait for self reception: %v", err)
	}
	if transmitted.Direction != gocan.DirectionTransmit || received.Direction != gocan.DirectionReceive {
		t.Fatalf("self-reception directions = %v, %v, want TX then RX", transmitted.Direction, received.Direction)
	}
}

func TestCaptureNamesBusesAndAssignsTheirIDs(t *testing.T) {
	capture := gocan.NewCapture()
	var network Network
	open := func(capture *gocan.Capture, config Config) *Bus {
		t.Helper()
		bus, err := network.Open(context.Background(), capture, config)
		if err != nil {
			t.Fatalf("Open %q: %v", config.Name, err)
		}
		t.Cleanup(func() { _ = bus.Close() })
		return bus
	}

	powertrain := open(capture, Config{Name: "powertrain"})
	chassis := open(capture, Config{ID: 3, Name: "chassis"})
	body := open(capture, Config{Name: "body"})
	diagnostics := open(capture, Config{Name: "diagnostics"})
	if powertrain.ID() != 1 || chassis.ID() != 3 || body.ID() != 2 || diagnostics.ID() != 4 {
		t.Fatalf("IDs = %d, %d, %d, %d; want 1, 3, 2, 4", powertrain.ID(), chassis.ID(), body.ID(), diagnostics.ID())
	}

	for _, config := range []Config{{ID: 1, Name: "renamed"}, {ID: 5, Name: "body"}} {
		if _, err := network.Open(context.Background(), capture, config); err == nil {
			t.Fatalf("Open %+v accepted a conflicting name and ID", config)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := network.Open(canceled, capture, Config{Name: "never opened"}); err == nil {
		t.Fatal("Open accepted a canceled context")
	}
	if _, ok := capture.BusID("never opened"); ok {
		t.Fatal("a failed Open named a bus in the capture")
	}

	if err := body.Close(); err != nil {
		t.Fatalf("Close body: %v", err)
	}
	capture.Clear()
	body = open(capture, Config{Name: "body"})
	if body.ID() != 2 {
		t.Fatalf("reopened body ID = %d, want its earlier ID 2", body.ID())
	}
	want := map[gocan.BusID]string{1: "powertrain", 2: "body", 3: "chassis", 4: "diagnostics"}
	if got := capture.BusNames(); !maps.Equal(got, want) {
		t.Fatalf("BusNames = %v, want %v", got, want)
	}
	if id, ok := capture.BusID("chassis"); !ok || id != 3 {
		t.Fatalf("BusID(chassis) = %d, %t; want 3, true", id, ok)
	}

	// Each capture numbers its own buses, even on a shared network.
	other := gocan.NewCapture()
	tester := open(other, Config{Name: "tester"})
	if tester.ID() != 1 {
		t.Fatalf("tester ID on its own capture = %d, want 1", tester.ID())
	}
	frame, err := gocan.NewFrame(0x7e0, []byte{2, 0x10, 3}, 0)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	cursor := capture.End()
	if err := tester.Send(context.Background(), frame); err != nil {
		t.Fatalf("tester Send: %v", err)
	}
	ctx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if _, _, err := capture.Next(ctx, gocan.FrameKey{Bus: body.ID(), ID: frame.ID, Direction: gocan.DirectionReceive}, cursor); err != nil {
		t.Fatalf("body did not receive the tester's frame: %v", err)
	}
}
