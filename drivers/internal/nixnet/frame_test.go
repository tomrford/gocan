package nixnet

import (
	"encoding/binary"
	"github.com/tomrford/gocan"
	"testing"
)

func TestFrameFormat(t *testing.T) {
	for _, fd := range []bool{false, true} {
		for _, extended := range []bool{false, true} {
			for dlc := uint8(0); dlc <= 15; dlc++ {
				if !fd && dlc > 8 {
					continue
				}
				flags := gocan.FrameFlags(0)
				if fd {
					flags |= gocan.FrameFD | gocan.FrameBitRateSwitch
				}
				if extended {
					flags |= gocan.FrameExtended
				}
				frame := gocan.Frame{ID: 0x123, DLC: dlc, Flags: flags}
				for i := 0; i < frame.DataLength(); i++ {
					frame.Data[i] = byte(i + 1)
				}
				encoded, size, err := encodeFrame(frame, fd)
				if err != nil {
					t.Fatal(err)
				}
				if size != recordSize(frame.DataLength()) || encoded[15] != byte(frame.DataLength()) {
					t.Fatalf("incorrect record size %d, length %d", size, encoded[15])
				}
				decoded, err := decodeFrame(encoded[:size])
				if err != nil || decoded != frame {
					t.Fatalf("round trip dlc=%d flags=%x: %+v %v", dlc, flags, decoded, err)
				}
			}
		}
	}
	for dlc := uint8(0); dlc <= 8; dlc++ {
		frame, _ := gocan.NewRemoteFrame(0x123, dlc, true)
		data, size, err := encodeFrame(frame, false)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeFrame(data[:size])
		if err != nil || decoded != frame || size != 24 {
			t.Fatalf("remote DLC %d: %+v, %v", dlc, decoded, err)
		}
	}
	frame, _ := gocan.NewFrame(0x123, []byte{1}, 0)
	data, size, err := encodeFrame(frame, true)
	if err != nil || data[12] != frameCAN20 {
		t.Fatalf("classic on FD: %v type %x", err, data[12])
	}
	if decoded, err := decodeFrame(data[:size]); err != nil || decoded != frame {
		t.Fatal(decoded, err)
	}
	for _, bad := range []gocan.Frame{{ID: 0x800}, {ID: 1, DLC: 9}, {ID: 1, Flags: gocan.FrameFD | gocan.FrameErrorStateIndicator}, {ID: 1, DLC: 15, Flags: gocan.FrameRemote}} {
		if _, _, err := encodeFrame(bad, true); err == nil {
			t.Fatalf("accepted unrepresentable frame %+v", bad)
		}
	}
	data[12] = frameFD
	data[15] = 64
	if _, err := decodeFrame(data[:24]); err == nil {
		t.Fatal("accepted truncated FD payload")
	}
	data[15] = 9
	if _, err := decodeFrame(data[:32]); err == nil {
		t.Fatal("accepted noncanonical FD length")
	}
	data[15] = 0
	binary.LittleEndian.PutUint32(data[8:], 0x800)
	if _, err := decodeFrame(data[:24]); err == nil {
		t.Fatal("accepted invalid identifier")
	}
}

func TestControllerStates(t *testing.T) {
	for _, test := range []struct {
		state, tx, rx byte
		want          gocan.ControllerState
		known         bool
	}{{0, 0, 0, gocan.ControllerActive, true}, {0, 96, 0, gocan.ControllerWarning, true}, {1, 128, 0, gocan.ControllerPassive, true}, {2, 255, 2, gocan.ControllerBusOff, false}} {
		event, err := controllerEvent(1, test.state, test.tx, test.rx)
		if err != nil || event.ControllerState != test.want || event.ErrorCountsKnown != test.known {
			t.Fatalf("state mapping %+v: %+v %v", test, event, err)
		}
	}
	if _, err := controllerEvent(1, 3, 0, 0); err == nil {
		t.Fatal("INIT must not be reported as active")
	}
}
