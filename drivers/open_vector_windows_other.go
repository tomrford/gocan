//go:build windows && !amd64

package drivers

import (
	"context"
	"fmt"

	"github.com/tomrford/gocan"
)

func openVector(context.Context, *gocan.Capture, Channel, Config, configMode) (gocan.Bus, error) {
	return nil, fmt.Errorf("Vector driver is unavailable on this Windows architecture")
}
