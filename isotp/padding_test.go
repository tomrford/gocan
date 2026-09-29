package isotp_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/isotp"
)

func TestSingleFramePadding(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var network virtual.Network
	capture := gocan.NewCapture()
	bus, err := network.Open(t.Context(), capture, virtual.Config{ID: 1, Name: "sender"})
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	for _, test := range []struct {
		maximum, minimum           uint8
		payloadLength, frameLength int
	}{
		{0, 0, 2, 3}, // Default classical framing stays unpadded.
		{8, 8, 2, 8},
		{64, 0, 2, 3}, // FD without optional padding.
		{64, 1, 2, 3},
		{64, 0, 8, 12},
		{12, 8, 10, 12},
		{64, 8, 2, 8},
		{64, 8, 7, 8},
		{64, 8, 8, 12},
		{64, 8, 11, 16},
		{64, 8, 15, 20},
		{64, 8, 19, 24},
		{64, 8, 23, 32},
		{64, 8, 31, 48},
		{64, 8, 47, 64},
		{64, 8, 62, 64},
		{64, 16, 8, 16},
	} {
		t.Run(fmt.Sprintf("max%d/min%d/payload%d", test.maximum, test.minimum, test.payloadLength), func(t *testing.T) {
			flags := gocan.FrameExtended
			if test.maximum > 8 {
				flags |= gocan.FrameFD | gocan.FrameBitRateSwitch
			}
			link, err := isotp.New(bus, isotp.Config{TransmitID: 0x18da10f1, ReceiveID: 0x18daf110, FrameFlags: flags, TransmitDataLength: test.maximum, TransmitMinDataLength: test.minimum, PaddingByte: 0xcc})
			if err != nil {
				t.Fatal(err)
			}
			functional, err := isotp.NewFunctional(bus, isotp.FunctionalConfig{TransmitID: 0x18db33f1, FrameFlags: flags, TransmitDataLength: test.maximum, TransmitMinDataLength: test.minimum, PaddingByte: 0xcc})
			if err != nil {
				t.Fatal(err)
			}
			payload := patternedPayload(test.payloadLength, 0xa5)
			// Explicit wire layout avoids validating one gocan encoder with another.
			header := []byte{byte(test.payloadLength)}
			if test.frameLength > 8 {
				header = []byte{0, byte(test.payloadLength)}
			}
			want := bytes.Repeat([]byte{0xcc}, test.frameLength)
			copy(want, header)
			copy(want[len(header):], payload)
			for _, path := range []struct {
				id   uint32
				send func(context.Context, []byte) error
			}{{0x18da10f1, link.Send}, {0x18db33f1, functional.Send}} {
				cursor := capture.End()
				if err := path.send(ctx, payload); err != nil {
					t.Fatal(err)
				}
				event, _, err := capture.Next(ctx, gocan.FrameKey{Bus: 1, ID: path.id, Direction: gocan.DirectionTransmit, Extended: true}, cursor)
				if err != nil {
					t.Fatal(err)
				}
				frame := event.Frame
				if frame.Flags != flags || frame.DataLength() != test.frameLength || !bytes.Equal(frame.Data[:frame.DataLength()], want) {
					t.Fatalf("%#x: frame = %+v, want flags %v and data %x", path.id, frame, flags, want)
				}
			}
		})
	}
}

func TestInvalidTransmitMinimum(t *testing.T) {
	var network virtual.Network
	bus, err := network.Open(t.Context(), gocan.NewCapture(), virtual.Config{ID: 1, Name: "sender"})
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	for _, test := range []struct {
		maximum, minimum uint8
		flags            gocan.FrameFlags
	}{
		{0, 12, 0},
		{8, 12, gocan.FrameFD},
		{64, 9, gocan.FrameFD},
		{64, 65, gocan.FrameFD},
	} {
		if _, err := isotp.New(bus, isotp.Config{TransmitID: 0x700, ReceiveID: 0x708, FrameFlags: test.flags, TransmitDataLength: test.maximum, TransmitMinDataLength: test.minimum}); err == nil {
			t.Errorf("New accepted %+v", test)
		}
		if _, err := isotp.NewFunctional(bus, isotp.FunctionalConfig{TransmitID: 0x7df, FrameFlags: test.flags, TransmitDataLength: test.maximum, TransmitMinDataLength: test.minimum}); err == nil {
			t.Errorf("NewFunctional accepted %+v", test)
		}
	}
}
