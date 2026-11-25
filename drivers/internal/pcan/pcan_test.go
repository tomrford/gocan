package pcan

import (
	"math"
	"testing"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
)

// TestClassicalBitrateTable checks every predefined code against the SJA1000
// timing it encodes: the bit rate is 8 MHz / ((BRP+1) * (3 + TSEG1 + TSEG2)),
// and the table key is that rate rounded to the nearest bit per second.
func TestClassicalBitrateTable(t *testing.T) {
	for bitsPerSecond, code := range classicalBitrates {
		brp := (uint32(code>>8) & 0x3f) + 1
		tseg1 := uint32(code) & 0x0f
		tseg2 := uint32(code>>4) & 0x07
		encoded := 8_000_000 / float64(brp*(3+tseg1+tseg2))
		if got := uint32(math.Round(encoded)); got != bitsPerSecond {
			t.Errorf("BTR0BTR1 %#04x encodes %d bit/s but is keyed as %d", code, got, bitsPerSecond)
		}
	}
}

func TestNativeLayouts(t *testing.T) {
	if got := unsafe.Sizeof(pcanMsg{}); got != 16 {
		t.Errorf("TPCANMsg size = %d, want 16", got)
	}
	if got := unsafe.Offsetof(pcanMsg{}.data); got != 6 {
		t.Errorf("TPCANMsg DATA offset = %d, want 6", got)
	}
	if got := unsafe.Sizeof(pcanMsgFD{}); got != 72 {
		t.Errorf("TPCANMsgFD size = %d, want 72", got)
	}
	if got := unsafe.Offsetof(pcanMsgFD{}.data); got != 6 {
		t.Errorf("TPCANMsgFD DATA offset = %d, want 6", got)
	}
	if got := unsafe.Sizeof(pcanChannelInformation{}); got != 52 {
		t.Errorf("TPCANChannelInformation size = %d, want 52", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.deviceType); got != 2 {
		t.Errorf("TPCANChannelInformation device type offset = %d, want 2", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.controllerNumber); got != 3 {
		t.Errorf("TPCANChannelInformation controller number offset = %d, want 3", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.deviceFeatures); got != 4 {
		t.Errorf("TPCANChannelInformation device features offset = %d, want 4", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.deviceName); got != 8 {
		t.Errorf("TPCANChannelInformation device name offset = %d, want 8", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.deviceID); got != 44 {
		t.Errorf("TPCANChannelInformation device ID offset = %d, want 44", got)
	}
	if got := unsafe.Offsetof(pcanChannelInformation{}.channelCondition); got != 48 {
		t.Errorf("TPCANChannelInformation channel condition offset = %d, want 48", got)
	}
}

func TestPCANEventTranslation(t *testing.T) {
	// Receive ESI comes from the controller, so normal hardware sends cannot
	// guarantee exercising this native flag.
	fd, err := decodePCANReceive(0x123, 0x1c, 10, []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, true, pcanStatusOK, 1)
	want := gocan.Frame{ID: 0x123, DLC: 10, Flags: gocan.FrameFD | gocan.FrameBitRateSwitch | gocan.FrameErrorStateIndicator}
	copy(want.Data[:], []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})
	if err != nil || !fd.hasFrame || fd.frame != want {
		t.Fatalf("FD receive flags = %+v, %v; want %+v", fd, err, want)
	}

	errorFrame, err := decodePCANReceive(
		0x08,
		pcanMessageError,
		4,
		[]byte{0x00, 0x19, 0x00, 0x80},
		false,
		pcanStatusOK,
		1,
	)
	if err != nil || errorFrame.eventCount != 2 ||
		errorFrame.events[0].Kind != gocan.EventErrorFrame {
		t.Fatalf("error frame = %+v, %v", errorFrame, err)
	}
	state := errorFrame.events[1]
	if state.Kind != gocan.EventControllerState ||
		state.ControllerState != gocan.ControllerPassive ||
		state.TXErrorCount != 128 || state.RXErrorCount != 0 ||
		!state.ErrorCountsKnown {
		t.Fatalf("error-frame state = %+v", errorFrame)
	}

	// Bench PCAN-USB status payloads at bus-off: 0x14 and 0x18 (November 2026).
	for _, payload := range []byte{0x10, 0x14, 0x18} {
		busOff, err := decodePCANReceive(
			1,
			pcanMessageStatus,
			4,
			[]byte{0x00, 0x00, 0x00, payload},
			false,
			pcanStatus(payload),
			1,
		)
		if err != nil || busOff.eventCount != 1 ||
			busOff.events[0].ControllerState != gocan.ControllerBusOff {
			t.Fatalf("bus-off status %#x = %+v, %v", payload, busOff, err)
		}
	}

	combined, err := decodePCANStatus(pcanStatusOverrun|pcanStatusBusOff, 1)
	if err != nil || combined.eventCount != 2 ||
		combined.events[0].Kind != gocan.EventReceiveOverrun ||
		combined.events[1].ControllerState != gocan.ControllerBusOff {
		t.Fatalf("overrun with bus-off = %+v, %v", combined, err)
	}

	if _, err := decodePCANStatus(pcanStatusInvalidHW, 1); err == nil {
		t.Fatal("decodePCANStatus accepted PCAN_ERROR_ILLHW")
	}
}

// TestPCANStateBenchSequences replays the controller-state sequences observed
// on classic PCAN-USB hardware without requiring attached adapters.
func TestPCANStateBenchSequences(t *testing.T) {
	now := time.Unix(100, 0)
	var state pcanState

	// 1. Send hits BUSOFF with no status frame: write gate latches, queued
	// passive error frames cannot release it, and live 0x14 -> 0x08 -> 0x00
	// polls release writes at 0x08 and reach Active at 0x00.
	event, ok := state.observeSendBusOff(now, 1)
	if !ok || event.ControllerState != gocan.ControllerBusOff || !state.busOff() {
		t.Fatalf("send bus-off = %+v, ok=%v, busOff=%v", event, ok, state.busOff())
	}
	passiveObs, err := decodePCANErrorFrame([]byte{0, 0, 129, 0}, pcanStatusOK, 1)
	if err != nil {
		t.Fatal(err)
	}
	events, count := state.observe(now.Add(time.Millisecond), passiveObs)
	if count != 1 || events[0].Kind != gocan.EventErrorFrame || !state.busOff() {
		t.Fatalf("queued error during bus-off = %+v, busOff=%v", events[:count], state.busOff())
	}
	polled, pcount, err := state.pollStatus(now.Add(busOffPollInterval), pcanStatusBusOff|pcanStatusBusLight, 1)
	if err != nil || pcount != 0 || !state.busOff() {
		t.Fatalf("poll 0x14 = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
	}
	polled, pcount, err = state.pollStatus(now.Add(2*busOffPollInterval), pcanStatusBusWarning, 1)
	if err != nil || pcount != 1 || polled[0].ControllerState != gocan.ControllerWarning || state.busOff() {
		t.Fatalf("poll 0x08 = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
	}
	// Duplicate 0x08 status, queued degraded/low-counter error frames, queued OK
	// status, and clean RX must neither cancel post-bus-off polling nor report
	// Active before a live pollStatus(0x00) confirms recovery.
	heavyObs, _ := decodePCANStatus(pcanStatusBusWarning, 1)
	state.observe(now.Add(2*busOffPollInterval+time.Millisecond), heavyObs)
	warnErrObs, err := decodePCANErrorFrame([]byte{0, 0, 0, 100}, pcanStatusOK, 1)
	if err != nil {
		t.Fatal(err)
	}
	state.observe(now.Add(2*busOffPollInterval+2*time.Millisecond), warnErrObs)
	lowErrObs, err := decodePCANErrorFrame([]byte{0, 0, 0, 8}, pcanStatusOK, 1)
	if err != nil {
		t.Fatal(err)
	}
	if events, count = state.observe(now.Add(2*busOffPollInterval+3*time.Millisecond), lowErrObs); count != 1 || events[0].Kind != gocan.EventErrorFrame {
		t.Fatalf("low-counter error during recovery poll = %+v", events[:count])
	}
	okObs, _ := decodePCANStatus(pcanStatusOK, 1)
	if events, count = state.observe(now.Add(2*busOffPollInterval+4*time.Millisecond), okObs); count != 0 {
		t.Fatalf("queued OK status during recovery poll = %+v", events[:count])
	}
	rxObs := pcanReceiveObservation{hasFrame: true, frame: gocan.Frame{ID: 0x100}}
	state.observe(now.Add(2*busOffPollInterval+5*time.Millisecond), rxObs)
	if state.nextDeadline().IsZero() {
		t.Fatal("queued status/error or duplicate 0x08 cancelled post-bus-off poll")
	}
	if _, ok := state.tick(now.Add(time.Second), 1); ok {
		t.Fatal("quiet timer inferred Active while post-bus-off polling remained active")
	}
	polled, pcount, err = state.pollStatus(now.Add(3*busOffPollInterval), pcanStatusOK, 1)
	if err != nil || pcount != 1 || polled[0].ControllerState != gocan.ControllerActive || !state.nextDeadline().IsZero() {
		t.Fatalf("poll 0x00 = %+v, err=%v, next=%v", polled[:pcount], err, state.nextDeadline())
	}

	// 2. Non-bus-off overrun on pollStatus releases the write gate, reports
	// overrun, and resets pollOverrun when polling stops so a later recovery
	// reports its own overrun.
	_, _ = state.observeSendBusOff(now.Add(time.Second), 1)
	polled, pcount, err = state.pollStatus(now.Add(time.Second+busOffPollInterval), pcanStatusQueueOverrun, 1)
	if err != nil || state.busOff() || pcount != 2 ||
		polled[0].Kind != gocan.EventReceiveOverrun || polled[1].ControllerState != gocan.ControllerActive {
		t.Fatalf("poll QOVERRUN = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
	}
	state.observe(now.Add(time.Second+2*busOffPollInterval), rxObs)
	_, _ = state.observeSendBusOff(now.Add(time.Second+3*busOffPollInterval), 1)
	polled, pcount, err = state.pollStatus(now.Add(time.Second+4*busOffPollInterval), pcanStatusQueueOverrun, 1)
	if err != nil || state.busOff() || pcount != 2 ||
		polled[0].Kind != gocan.EventReceiveOverrun || polled[1].ControllerState != gocan.ControllerActive {
		t.Fatalf("second recovery poll QOVERRUN = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
	}

	// 3. Passive ignores ambiguous BUSHEAVY (0x08) while resetting the quiet
	// window, accepts BUSLIGHT (0x04), stays passive without clean RX, and
	// infers Active after clean RX followed by degradedQuietWindow without errors.
	t0 := now.Add(2 * time.Second)
	events, count = state.observe(t0, passiveObs)
	if count != 2 || events[1].ControllerState != gocan.ControllerPassive {
		t.Fatalf("enter passive = %+v", events[:count])
	}
	state.observe(t0.Add(time.Millisecond), rxObs)
	if events, count = state.observe(t0.Add(2*time.Millisecond), heavyObs); count != 0 {
		t.Fatalf("BUSHEAVY downgraded passive: %+v", events[:count])
	}
	if _, ok := state.tick(t0.Add(time.Second), 1); ok {
		t.Fatal("BUSHEAVY did not reset the quiet window after clean RX")
	}
	lightObs, _ := decodePCANStatus(pcanStatusBusLight, 1)
	if events, count = state.observe(t0.Add(3*time.Millisecond), lightObs); count != 1 || events[0].ControllerState != gocan.ControllerWarning {
		t.Fatalf("BUSLIGHT after passive = %+v", events[:count])
	}
	state.observe(t0.Add(10*time.Millisecond), rxObs)
	if _, ok := state.tick(t0.Add(10*time.Millisecond+degradedQuietWindow-time.Millisecond), 1); ok {
		t.Fatal("inferred Active before degradedQuietWindow elapsed")
	}
	if event, ok = state.tick(t0.Add(10*time.Millisecond+degradedQuietWindow), 1); !ok || event.ControllerState != gocan.ControllerActive {
		t.Fatalf("quiet-window Active = %+v, ok=%v", event, ok)
	}
}
