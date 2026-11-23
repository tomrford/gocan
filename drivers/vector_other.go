//go:build !(windows && amd64)

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/vector"
)

// The XL Driver Library ships as vxlapi64.dll, so the Vector driver exists
// only on windows/amd64.
func discoverVector() ([]Channel, error) { return nil, nil }

func openVector(context.Context, *gocan.Capture, vector.Config) (gocan.Bus, error) {
	return nil, fmt.Errorf("%w: vector driver is unavailable on this platform", gocan.ErrDriverUnavailable)
}
