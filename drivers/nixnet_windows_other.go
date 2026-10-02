//go:build windows && !amd64

package drivers

import (
	"context"
	"fmt"
	"github.com/tomrford/gocan"
)

// The floating-point stream ABI is currently qualified on windows/amd64 only.
func discoverNIXNET() ([]Channel, error) { return nil, nil }
func openNIXNET(context.Context, *gocan.Capture, Channel, Config, bool) (gocan.Bus, error) {
	return nil, fmt.Errorf("NI-XNET is unavailable on this Windows architecture")
}
