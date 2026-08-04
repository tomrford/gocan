//go:build !amd64

package drivers

// The XL Driver Library ships as vxlapi64.dll, so the Vector driver exists
// only on windows/amd64; other Windows architectures have no Vector channels.
func discoverVector() ([]Channel, error) {
	return nil, nil
}
