//go:build !linux && !windows

package drivers

import (
	"context"
	"errors"

	"github.com/tomrford/gocan"
)

func openChannel(context.Context, *gocan.Capture, Channel, Config) (gocan.Bus, error) {
	return nil, errors.New("drivers: no physical CAN driver is available on this platform")
}
