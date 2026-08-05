//go:build windows

package drivers

import (
	"context"
	"strings"
	"testing"

	"github.com/tomrford/gocan"
)

func TestOpenDispatchRejectsAChannelFromAnotherPlatform(t *testing.T) {
	channel := Channel{driver: driverSocketCAN, name: "can0", nativeName: "can0", external: true}
	_, err := Open(context.Background(), gocan.NewCapture(), channel, Config{ID: 1, Name: "can", External: true})
	if err == nil || !strings.Contains(err.Error(), "not available on Windows") {
		t.Fatalf("Open error = %v, want platform dispatch rejection", err)
	}
}

func TestPCANConfigTranslation(t *testing.T) {
	channel := Channel{driver: driverPCAN, native: 0x51, supportsFD: true}
	classic, err := nativePCANConfig(channel, Config{ID: 1, Name: "can", Bitrate: 500_000}, modeClassic)
	if err != nil || classic.Bitrate == 0 || classic.FDBitrate != "" {
		t.Fatalf("classic native config = %+v, %v", classic, err)
	}
	if _, err := nativePCANConfig(channel, Config{ID: 1, Name: "can", Bitrate: 250_000}, modeClassic); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported bitrate error = %v", err)
	}
	fd, err := nativePCANConfig(channel, Config{ID: 1, Name: "can", FDTiming: qualifiedFDTiming}, modeFD)
	if err != nil {
		t.Fatalf("FD native config: %v", err)
	}
	want := "f_clock=80000000, nom_brp=1, nom_tseg1=119, nom_tseg2=40, nom_sjw=1, data_brp=1, data_tseg1=29, data_tseg2=10, data_sjw=1"
	if fd.FDBitrate != want || fd.Bitrate != 0 {
		t.Fatalf("FD timing = %q, bitrate %#x; want %q, zero", fd.FDBitrate, fd.Bitrate, want)
	}
}
