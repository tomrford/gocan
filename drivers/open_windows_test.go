//go:build windows

package drivers

import (
	"strings"
	"testing"

	"github.com/tomrford/gocan/drivers/internal/pcan"
)

func TestPCANConfigTranslation(t *testing.T) {
	channel := Channel{driver: driverPCAN, native: 0x51, supportsFD: true}
	classic, err := nativePCANConfig(channel, Config{ID: 1, Name: "can", Bitrate: 500_000}, false)
	if err != nil || classic.Bitrate != pcan.Bitrate500K || classic.FDBitrate != "" {
		t.Fatalf("classic native config = %+v, %v", classic, err)
	}
	if _, err := nativePCANConfig(channel, Config{ID: 1, Name: "can", Bitrate: 250_000}, false); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported bitrate error = %v", err)
	}
	fd, err := nativePCANConfig(channel, Config{ID: 1, Name: "can", FDTiming: qualifiedFDTiming}, true)
	if err != nil {
		t.Fatalf("FD native config: %v", err)
	}
	want := "f_clock=80000000, nom_brp=1, nom_tseg1=119, nom_tseg2=40, nom_sjw=1, data_brp=1, data_tseg1=29, data_tseg2=10, data_sjw=1"
	if fd.FDBitrate != want || fd.Bitrate != 0 {
		t.Fatalf("FD timing = %q, bitrate %#x; want %q, zero", fd.FDBitrate, fd.Bitrate, want)
	}
}
