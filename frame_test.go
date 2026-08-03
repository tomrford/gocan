package gocan

import "testing"

func TestDLCLengthRoundTrip(t *testing.T) {
	for _, fd := range []bool{false, true} {
		for dlc := uint8(0); dlc <= 15; dlc++ {
			length, err := DLCToLength(dlc, fd)
			if err != nil {
				t.Fatalf("DLCToLength(%d, fd=%t): %v", dlc, fd, err)
			}
			back, err := LengthToDLC(length, fd)
			if err != nil {
				t.Fatalf("LengthToDLC(%d, fd=%t): %v", length, fd, err)
			}
			want := dlc
			if !fd && dlc > 8 {
				// Classical DLCs above 8 all describe eight payload bytes.
				want = 8
			}
			if back != want {
				t.Errorf("dlc %d (fd=%t) -> length %d -> dlc %d, want %d", dlc, fd, length, back, want)
			}
		}

		for length := 0; length <= MaxDataLength; length++ {
			dlc, err := LengthToDLC(length, fd)
			if err != nil {
				// Lengths with no exact DLC must be rejected, never rounded.
				continue
			}
			back, err := DLCToLength(dlc, fd)
			if err != nil || back != length {
				t.Errorf("length %d (fd=%t) -> dlc %d -> length %d (%v)", length, fd, dlc, back, err)
			}
		}
	}

	if _, err := DLCToLength(16, true); err == nil {
		t.Error("DLCToLength(16, fd=true) succeeded, want error")
	}
	if _, err := LengthToDLC(9, false); err == nil {
		t.Error("LengthToDLC(9, fd=false) succeeded, want error")
	}
}

func TestRemoteFrameCarriesNoPayload(t *testing.T) {
	frame, err := NewRemoteFrame(0x123, 8, false)
	if err != nil {
		t.Fatalf("NewRemoteFrame: %v", err)
	}
	if got := frame.DataLength(); got != 0 {
		t.Errorf("remote frame DataLength() = %d, want 0", got)
	}
	if frame.DLC != 8 {
		t.Errorf("remote frame DLC = %d, want the requested data length 8", frame.DLC)
	}
	if _, err := NewFrame(0x123, nil, FrameRemote); err == nil {
		t.Error("NewFrame accepted FrameRemote, want rejection")
	}
}
