//go:build windows

package drivers

import (
	"testing"
)

func TestPCANConfigTranslation(t *testing.T) {
	channel := Channel{driver: driverPCAN, native: 0x51, supportsFD: true}
	fd := nativePCANConfig(channel, Config{ID: 1, Name: "can", FDTiming: qualifiedFDTiming}, true)
	want := "f_clock=80000000, nom_brp=1, nom_tseg1=119, nom_tseg2=40, nom_sjw=1, data_brp=1, data_tseg1=29, data_tseg2=10, data_sjw=1"
	if fd.FDBitrate != want || fd.Bitrate != 0 {
		t.Fatalf("FD timing = %q, bitrate %#x; want %q, zero", fd.FDBitrate, fd.Bitrate, want)
	}
}
