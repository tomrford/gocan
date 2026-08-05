//go:build windows && amd64

package drivers

import "testing"

func TestVectorFDConfigHasNoTimingDefaults(t *testing.T) {
	channel := Channel{driver: driverVector, native: 2, supportsFD: true}
	native, err := nativeVectorConfig(channel, Config{ID: 1, Name: "can", FDTiming: qualifiedFDTiming}, modeFD)
	if err != nil {
		t.Fatalf("nativeVectorConfig: %v", err)
	}
	if native.Bitrate != 0 || native.FDTiming.ArbitrationBitrate != 500_000 || native.FDTiming.DataBitrate != 2_000_000 {
		t.Fatalf("native bitrates = %+v", native)
	}
	if got := native.FDTiming.Arbitration; got.SJW != 1 || got.TSEG1 != 119 || got.TSEG2 != 40 {
		t.Fatalf("native nominal timing = %+v", got)
	}
	if got := native.FDTiming.Data; got.SJW != 1 || got.TSEG1 != 29 || got.TSEG2 != 10 {
		t.Fatalf("native data timing = %+v", got)
	}
}
