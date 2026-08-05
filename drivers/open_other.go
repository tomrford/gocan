//go:build !linux && !windows

package drivers

import (
	"context"
	"errors"

	"github.com/tomrford/gocan"
)

// Open reports that this platform has no physical CAN driver.
func Open(context.Context, *gocan.Capture, Channel, Config) (gocan.Bus, error) {
	return nil, errors.New("no physical CAN driver is available on this platform")
}
