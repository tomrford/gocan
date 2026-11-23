package drivers

import "testing"

func TestPCANConfigTranslation(t *testing.T) {
	channel := PCANTestChannel(true)
	fd, err := nativePCANConfig(channel, preparedConfig{
		config: Config{
			ID:   1,
			Name: "can",
			FDTiming: FDTiming{
				ClockHz: 80_000_000,
				Nominal: BitTiming{BRP: 1, TSEG1: 119, TSEG2: 40, SJW: 1},
				Data:    BitTiming{BRP: 1, TSEG1: 29, TSEG2: 10, SJW: 1},
			},
		},
		fd: true,
	})
	if err != nil {
		t.Fatalf("nativePCANConfig: %v", err)
	}
	want := "f_clock=80000000, nom_brp=1, nom_tseg1=119, nom_tseg2=40, nom_sjw=1, data_brp=1, data_tseg1=29, data_tseg2=10, data_sjw=1"
	if fd.FDBitrate != want || fd.Bitrate != 0 {
		t.Fatalf("FD timing = %q, bitrate %#x; want %q, zero", fd.FDBitrate, fd.Bitrate, want)
	}
}
