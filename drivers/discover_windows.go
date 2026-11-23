//go:build windows

package drivers

import (
	"errors"

	"github.com/tomrford/gocan"
)

var windowsDrivers = [...]*driverDescriptor{
	&pcanDriver,
	&vectorDriver,
	&nixnetDriver,
	&zlgDriver,
}

func discoverChannels() ([]Channel, error) {
	var (
		channels []Channel
		errs     []error
	)
	for _, driver := range windowsDrivers {
		found, err := driver.discoverChannels()
		if err != nil && !errors.Is(err, gocan.ErrDriverUnavailable) {
			errs = append(errs, err)
		}
		channels = append(channels, found...)
	}
	return channels, errors.Join(errs...)
}
