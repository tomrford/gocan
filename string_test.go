package gocan

import (
	"strings"
	"testing"
)

func testFrame(t *testing.T, id uint32, data []byte, flags FrameFlags) Frame {
	t.Helper()
	frame, err := NewFrame(id, data, flags)
	if err != nil {
		t.Fatalf("NewFrame(%#x, %d bytes, %v): %v", id, len(data), flags, err)
	}
	return frame
}

func testRemoteFrame(t *testing.T, id uint32, dlc uint8, extended bool) Frame {
	t.Helper()
	frame, err := NewRemoteFrame(id, dlc, extended)
	if err != nil {
		t.Fatalf("NewRemoteFrame(%#x, dlc %d): %v", id, dlc, err)
	}
	return frame
}

// rawDLCFrame builds a classical frame whose on-wire DLC is above eight, which
// only a frame received from a driver can carry.
func rawDLCFrame(dlc uint8) Frame {
	frame := Frame{ID: 0x123, DLC: dlc}
	copy(frame.Data[:8], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	return frame
}

func TestFrameString(t *testing.T) {
	tests := []struct {
		name  string
		frame Frame
		want  string
	}{
		{"standard data", testFrame(t, 0x123, []byte{0x11, 0x22}, 0), "123#1122"},
		{"standard without payload", testFrame(t, 0x123, nil, 0), "123#"},
		{"standard maximum identifier", testFrame(t, MaxStandardID, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0), "7FF#0102030405060708"},
		{"extended data", testFrame(t, 0x1abcde, []byte{0xaa}, FrameExtended), "001ABCDE#AA"},
		{"extended maximum identifier", testFrame(t, MaxExtendedID, []byte{0xff}, FrameExtended), "1FFFFFFF#FF"},
		{"can fd without flags", testFrame(t, 0x123, make([]byte, 64), FrameFD), "123##0" + strings.Repeat("00", 64)},
		{"can fd bit-rate switch", testFrame(t, 0x123, make([]byte, 12), FrameFD|FrameBitRateSwitch), "123##1" + strings.Repeat("00", 12)},
		{"can fd error state indicator", testFrame(t, 0x1abcde, []byte{1}, FrameExtended|FrameFD|FrameErrorStateIndicator), "001ABCDE##201"},
		{"can fd both flags", testFrame(t, 0x123, []byte{1}, FrameFD|FrameBitRateSwitch|FrameErrorStateIndicator), "123##301"},
		{"remote with data length", testRemoteFrame(t, 0x123, 4, false), "123#R4"},
		{"remote without data length", testRemoteFrame(t, 0x123, 0, false), "123#R"},
		{"remote extended", testRemoteFrame(t, 0x1abcde, 8, true), "001ABCDE#R8"},
		{"remote above eight", testRemoteFrame(t, 0x123, 9, false), "123#R9"},
		{"classical dlc nine", rawDLCFrame(9), "123#0102030405060708_9"},
		{"classical dlc fifteen", rawDLCFrame(15), "123#0102030405060708_F"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.frame.String(); got != test.want {
				t.Errorf("String() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFrameStringInvalidFrame(t *testing.T) {
	invalid := []Frame{
		{ID: MaxStandardID + 1},
		{ID: 0x100, DLC: 16},
		{ID: 0x100, Flags: FrameBitRateSwitch},
		{ID: 0x100, Flags: FrameRemote | FrameFD},
		{ID: 0x100, Flags: 1 << 7},
	}
	for _, frame := range invalid {
		got := frame.String()
		if !strings.HasPrefix(got, "Frame{") || !strings.HasSuffix(got, " (invalid)") {
			t.Errorf("String() of %#v = %q, want the diagnostic form", frame, got)
		}
	}
}
