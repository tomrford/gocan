//go:build !linux && !windows

package drivers

// Discover reports no channels: this platform has no physical CAN driver.
func Discover() ([]Channel, error) {
	return nil, nil
}
