//go:build !linux

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
)

func discoverSocketCAN() ([]Channel, error) { return nil, nil }

func openSocketCAN(context.Context, *gocan.Capture, socketcanPrepared) (gocan.Bus, error) {
	return nil, fmt.Errorf("%w: socketcan driver is unavailable on this platform", gocan.ErrDriverUnavailable)
}
