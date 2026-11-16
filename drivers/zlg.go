package drivers

// zlgFDRates are rate pairs that ZLG USBCANFD adapters configure from their
// own built-in timing, sampling at 80% except at 5 Mbit/s data (75%). ZLG
// channels accept no custom FDTiming.
var zlgFDRates = [...]FDRatePreset{
	{Bitrate: 500_000, DataBitrate: 2_000_000},
	{Bitrate: 500_000, DataBitrate: 4_000_000},
	{Bitrate: 500_000, DataBitrate: 5_000_000},
	{Bitrate: 1_000_000, DataBitrate: 2_000_000},
	{Bitrate: 1_000_000, DataBitrate: 4_000_000},
	{Bitrate: 1_000_000, DataBitrate: 5_000_000},
}

// A ZLG native selector packs the device type, device index and channel.
func zlgNative(deviceType, device, channel uint32) uint64 {
	return uint64(deviceType)<<48 | uint64(device&0xffff)<<32 | uint64(channel)
}

func zlgSelector(native uint64) (deviceType, device, channel uint32) {
	return uint32(native >> 48), uint32(native>>32) & 0xffff, uint32(native)
}
