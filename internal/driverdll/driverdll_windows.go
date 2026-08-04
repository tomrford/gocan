//go:build windows

// Package driverdll loads native CAN driver libraries from the Windows system
// directory.
package driverdll

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tomrford/gocan"
	"golang.org/x/sys/windows"
)

// LoadSystem loads name from the Windows system directory. A missing requested
// DLL is reported as ErrDriverUnavailable; other loader failures are preserved.
func LoadSystem(name string) (*windows.LazyDLL, error) {
	dll := windows.NewLazySystemDLL(name)
	if err := dll.Load(); err != nil {
		return nil, loadError(name, err)
	}
	return dll, nil
}

func loadError(name string, err error) error {
	if errors.Is(err, windows.ERROR_MOD_NOT_FOUND) {
		systemDirectory, directoryErr := windows.GetSystemDirectory()
		if directoryErr == nil {
			_, statErr := os.Stat(filepath.Join(systemDirectory, name))
			if os.IsNotExist(statErr) {
				return fmt.Errorf(
					"%w: load %s from the Windows system directory: %v",
					gocan.ErrDriverUnavailable,
					name,
					err,
				)
			}
		}
	}
	return fmt.Errorf("load %s from the Windows system directory: %w", name, err)
}
