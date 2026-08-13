package isotp_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/isotp"
)

func TestFunctionalBroadcast(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	tester, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		t.Fatalf("Open tester: %v", err)
	}
	t.Cleanup(func() { _ = tester.Close() })
	servers := make([]*isotp.Link, 2)
	for index := range servers {
		bus, err := network.Open(context.Background(), capture, virtual.Config{ID: gocan.BusID(index) + 2, Name: "ecu"})
		if err != nil {
			t.Fatalf("Open ECU %d: %v", index, err)
		}
		t.Cleanup(func() { _ = bus.Close() })
		servers[index], err = isotp.New(bus, capture, isotp.Config{TransmitID: 0x7e8 + uint32(index), ReceiveID: 0x7df})
		if err != nil {
			t.Fatalf("New server link %d: %v", index, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	functional, err := isotp.NewFunctional(tester, isotp.FunctionalConfig{TransmitID: 0x7df})
	if err != nil {
		t.Fatalf("NewFunctional: %v", err)
	}
	payload := patternedPayload(7, 0x3e)
	if err := functional.Send(ctx, payload); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for index, server := range servers {
		received, err := server.Receive(ctx)
		if err != nil || !bytes.Equal(received, payload) {
			t.Fatalf("server %d received %x, %v", index, received, err)
		}
	}

	if err := functional.Send(ctx, patternedPayload(8, 0x3e)); !errors.Is(err, isotp.ErrPayloadTooLarge) {
		t.Fatalf("oversized Send = %v, want ErrPayloadTooLarge", err)
	}

	padded, err := isotp.NewFunctional(tester, isotp.FunctionalConfig{
		TransmitID:         0x7df,
		FrameFlags:         gocan.FrameFD,
		TransmitDataLength: 64,
		PadFrames:          true,
		PaddingByte:        0xcc,
	})
	if err != nil {
		t.Fatalf("NewFunctional FD: %v", err)
	}
	long := patternedPayload(62, 0x2e)
	if err := padded.Send(ctx, long); err != nil {
		t.Fatalf("Send FD: %v", err)
	}
	received, err := servers[0].Receive(ctx)
	if err != nil || !bytes.Equal(received, long) {
		t.Fatalf("server received %x, %v", received, err)
	}
	if err := padded.Send(ctx, patternedPayload(63, 0x2e)); !errors.Is(err, isotp.ErrPayloadTooLarge) {
		t.Fatalf("oversized FD Send = %v, want ErrPayloadTooLarge", err)
	}
}
