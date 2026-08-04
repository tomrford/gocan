//go:build windows

package driverdll

import (
	"errors"
	"testing"

	"github.com/tomrford/gocan"
	"golang.org/x/sys/windows"
)

func TestLoadErrorOnlyMarksMissingDLLUnavailable(t *testing.T) {
	present := loadError("kernel32.dll", windows.ERROR_MOD_NOT_FOUND)
	if errors.Is(present, gocan.ErrDriverUnavailable) {
		t.Fatalf("present DLL error = %v, unexpectedly unavailable", present)
	}
	if !errors.Is(present, windows.ERROR_MOD_NOT_FOUND) {
		t.Fatalf("present DLL error = %v, want loader failure", present)
	}

	const missingName = "gocan-test-definitely-missing-driver.dll"
	missing := loadError(missingName, windows.ERROR_MOD_NOT_FOUND)
	if !errors.Is(missing, gocan.ErrDriverUnavailable) {
		t.Fatalf("missing DLL error = %v, want ErrDriverUnavailable", missing)
	}

	other := loadError(missingName, windows.ERROR_BAD_EXE_FORMAT)
	if errors.Is(other, gocan.ErrDriverUnavailable) {
		t.Fatalf("bad-image error = %v, unexpectedly unavailable", other)
	}
	if !errors.Is(other, windows.ERROR_BAD_EXE_FORMAT) {
		t.Fatalf("bad-image error = %v, want loader failure", other)
	}
}
