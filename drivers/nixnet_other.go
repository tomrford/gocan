//go:build !(windows && amd64)

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/nixnet"
)

// The floating-point stream ABI is currently qualified on windows/amd64 only.
func discoverNIXNET() ([]Channel, error) { return nil, nil }

func openNIXNET(context.Context, *gocan.Capture, nixnet.Config) (gocan.Bus, error) {
	return nil, fmt.Errorf("%w: nixnet driver is unavailable on this platform", gocan.ErrDriverUnavailable)
}
