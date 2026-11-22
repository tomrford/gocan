//go:build !linux && !windows

package drivers

func discoverChannels() ([]Channel, error) {
	return nil, nil
}
