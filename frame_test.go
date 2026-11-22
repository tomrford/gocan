package gocan_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/tomrford/gocan"
)

func TestDLCLengthMapping(t *testing.T) {
	fdLengths := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 20, 24, 32, 48, 64}
	for _, fd := range []bool{false, true} {
		for dlc, length := range fdLengths {
			wantDLC := dlc
			if !fd {
				length = min(length, 8)
				wantDLC = min(dlc, 8)
			}
			if got, err := gocan.DLCToLength(uint8(dlc), fd); err != nil || got != length {
				t.Errorf("DLCToLength(%d, fd=%t) = %d, %v; want %d", dlc, fd, got, err, length)
			}
			if got, err := gocan.LengthToDLC(length, fd); err != nil || int(got) != wantDLC {
				t.Errorf("LengthToDLC(%d, fd=%t) = %d, %v; want %d", length, fd, got, err, wantDLC)
			}
		}

		for length := -1; length <= gocan.MaxDataLength+1; length++ {
			if slices.Contains(fdLengths, length) && (fd || length <= 8) {
				continue
			}
			if _, err := gocan.LengthToDLC(length, fd); !errors.Is(err, gocan.ErrInvalidFrame) {
				t.Errorf("LengthToDLC(%d, fd=%t) error = %v, want %v", length, fd, err, gocan.ErrInvalidFrame)
			}
		}
	}

	if _, err := gocan.DLCToLength(16, true); !errors.Is(err, gocan.ErrInvalidFrame) {
		t.Errorf("DLCToLength(16, fd=true) error = %v, want %v", err, gocan.ErrInvalidFrame)
	}
}

func TestRemoteFrameCarriesNoPayload(t *testing.T) {
	frame, err := gocan.NewRemoteFrame(0x123, 8, false)
	if err != nil {
		t.Fatalf("NewRemoteFrame: %v", err)
	}
	if got := frame.DataLength(); got != 0 {
		t.Errorf("remote frame DataLength() = %d, want 0", got)
	}
	if frame.DLC != 8 {
		t.Errorf("remote frame DLC = %d, want the requested data length 8", frame.DLC)
	}
	if _, err := gocan.NewFrame(0x123, nil, gocan.FrameRemote); !errors.Is(err, gocan.ErrInvalidFrame) {
		t.Errorf("NewFrame(FrameRemote) error = %v, want %v", err, gocan.ErrInvalidFrame)
	}
}
