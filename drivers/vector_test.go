package drivers

import "testing"

func TestVectorFDConfigTranslation(t *testing.T) {
	channel := VectorTestChannel(true, false)
	native, err := nativeVectorConfig(channel, preparedConfig{
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

	// Verify every advertised preset resolves to native timing matching its advertised rates.
	for _, preset := range channel.FDRatePresets() {
		resolved, err := nativeVectorConfig(channel, preparedConfig{
			config: Config{ID: 1, Name: "can", Bitrate: preset.Bitrate, DataBitrate: preset.DataBitrate},
			fd:     true,
		})
		if err != nil {
			t.Fatalf("nativeVectorConfig preset %+v: %v", preset, err)
		}
		if resolved.Bitrate != 0 || resolved.FDTiming.ArbitrationBitrate != preset.Bitrate || resolved.FDTiming.DataBitrate != preset.DataBitrate {
			t.Fatalf("preset %+v resolved to %+v", preset, resolved)
		}
	}
}
