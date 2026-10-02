package drivers

import "fmt"

// NI-XNET uses 25 ns quanta (40 MHz). Keep the public clock/prescaler
// representation, translating equivalent quanta exactly rather than rounding.
var nixnetFDPresets = [...]fdPreset{
	{rate: FDRatePreset{Bitrate: 500_000, DataBitrate: 2_000_000}, timing: FDTiming{ClockHz: 40_000_000, Nominal: BitTiming{BRP: 1, TSEG1: 63, TSEG2: 16, SJW: 16}, Data: BitTiming{BRP: 1, TSEG1: 15, TSEG2: 4, SJW: 4}}},
	{rate: FDRatePreset{Bitrate: 500_000, DataBitrate: 4_000_000}, timing: FDTiming{ClockHz: 40_000_000, Nominal: BitTiming{BRP: 1, TSEG1: 63, TSEG2: 16, SJW: 16}, Data: BitTiming{BRP: 1, TSEG1: 7, TSEG2: 2, SJW: 2}}},
}

func nixnetFDTiming(timing FDTiming) (uint64, uint64, error) {
	if _, _, err := deriveFDBitrates(timing); err != nil {
		return 0, 0, err
	}
	nominal, err := nixnetBitTiming(timing.ClockHz, timing.Nominal, false)
	if err != nil {
		return 0, 0, err
	}
	data, err := nixnetBitTiming(timing.ClockHz, timing.Data, true)
	if err != nil {
		return 0, 0, err
	}
	return nominal, data, nil
}

func nixnetBitTiming(clock uint32, timing BitTiming, data bool) (uint64, error) {
	nanos := uint64(timing.BRP) * 1_000_000_000
	if clock == 0 || nanos%uint64(clock) != 0 {
		return 0, fmt.Errorf("NI-XNET timing requires integral time quanta in nanoseconds")
	}
	tq := nanos / uint64(clock)
	maxTQ, maxTSEG1, maxTSEG2, maxSJW := uint64(12800), uint32(256), uint32(128), uint32(128)
	minTSEG1 := uint32(2)
	phase := "nominal"
	if data {
		phase = "data"
		maxTQ, maxTSEG1, maxTSEG2, maxSJW = 800, 32, 16, 16
		minTSEG1 = 1
	}
	if tq < 25 || tq > maxTQ || tq%25 != 0 || timing.TSEG1 < minTSEG1 || timing.TSEG1 > maxTSEG1 || timing.TSEG2 == 0 || timing.TSEG2 > maxTSEG2 || timing.SJW == 0 || timing.SJW > maxSJW || timing.SJW > timing.TSEG2 {
		return 0, fmt.Errorf("NI-XNET timing exceeds %s phase limits or requires a quantum other than multiples of 25 ns", phase)
	}
	// Encode nxPropSession_IntfBaudRate64 and IntfCanFdBaudRate64.
	if data {
		encoded := uint64(0xa0000000) | tq<<13 | uint64(timing.TSEG1-1)<<8 | uint64(timing.TSEG2-1)<<4 | uint64(timing.SJW-1)
		// TDCO uses controller clock periods (25 ns), not prescaled quanta.
		// Enable measured loop-delay compensation for data rates above 1M.
		// https://knowledge.ni.com/KnowledgeArticleDetails?id=kA0VU0000002kGT0AY
		if tq*(1+uint64(timing.TSEG1)+uint64(timing.TSEG2)) < 1000 {
			offset := tq * (1 + uint64(timing.TSEG1)) / 25
			encoded |= uint64(1)<<55 | offset<<40
		}
		return encoded, nil
	}
	return 0xa0000000 | tq<<32 | uint64(timing.SJW-1)<<16 | uint64(timing.TSEG1-1)<<8 | uint64(timing.TSEG2-1), nil
}
