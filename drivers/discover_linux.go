//go:build linux

package drivers

func discoverChannels() ([]Channel, error) {
	return socketcanDriver.discoverChannels()
}
