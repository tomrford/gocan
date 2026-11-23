//go:build !windows

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/pcan"
)

func discoverPCAN() ([]Channel, error) { return nil, nil }

func openPCAN(context.Context, *gocan.Capture, pcan.Config) (gocan.Bus, error) {
	return nil, fmt.Errorf("%w: pcan driver is unavailable on this platform", gocan.ErrDriverUnavailable)
}
