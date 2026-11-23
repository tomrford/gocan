package transport

// CANFDLength returns the smallest legal CAN FD data length holding length
// bytes. Lengths up to 8 are unchanged and lengths above 64 return 64.
func CANFDLength(length int) int {
	switch {
	case length <= 8:
		return length
	case length <= 12:
		return 12
	case length <= 16:
		return 16
	case length <= 20:
		return 20
	case length <= 24:
		return 24
	case length <= 32:
		return 32
	case length <= 48:
		return 48
	default:
		return 64
	}
}
