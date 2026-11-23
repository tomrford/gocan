//go:build !windows

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/zlg"
)

func discoverZLG() ([]Channel, error) { return nil, nil }

func openZLG(context.Context, *gocan.Capture, zlg.Config) (gocan.Bus, error) {
	return nil, fmt.Errorf("%w: zlg driver is unavailable on this platform", gocan.ErrDriverUnavailable)
}
