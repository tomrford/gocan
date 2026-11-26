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
	// passive error frames cannot release it, live 0x14 -> 0x08 releases writes
	// into Warning, and live 0x00 must hold for degradedClearHold to reach Active.
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
	polled, pcount, err := state.pollStatus(now.Add(statusPollInterval), pcanStatusBusOff|pcanStatusBusLight, 1, true)
	if err != nil || pcount != 0 || !state.busOff() {
		t.Fatalf("poll 0x14 = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
	}
	polled, pcount, err = state.pollStatus(now.Add(2*statusPollInterval), pcanStatusBusWarning, 1, true)
	if err != nil || pcount != 1 || polled[0].ControllerState != gocan.ControllerWarning || state.busOff() {
		t.Fatalf("poll 0x08 = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
	}
	// Duplicate 0x08 status, queued degraded/low-counter error frames, queued OK
	// status, and clean RX must neither cancel post-bus-off polling nor report
	// Active before sustained live pollStatus(0x00) confirms recovery.
	heavyObs, _ := decodePCANStatus(pcanStatusBusWarning, 1)
	state.observe(now.Add(2*statusPollInterval+time.Millisecond), heavyObs)
	warnErrObs, err := decodePCANErrorFrame([]byte{0, 0, 0, 100}, pcanStatusOK, 1)
	if err != nil {
		t.Fatal(err)
	}
	state.observe(now.Add(2*statusPollInterval+2*time.Millisecond), warnErrObs)
	lowErrObs, err := decodePCANErrorFrame([]byte{0, 0, 0, 8}, pcanStatusOK, 1)
	if err != nil {
		t.Fatal(err)
	}
	if events, count = state.observe(now.Add(2*statusPollInterval+3*time.Millisecond), lowErrObs); count != 1 || events[0].Kind != gocan.EventErrorFrame {
		t.Fatalf("low-counter error during recovery poll = %+v", events[:count])
	}
	okObs, _ := decodePCANStatus(pcanStatusOK, 1)
	if events, count = state.observe(now.Add(2*statusPollInterval+4*time.Millisecond), okObs); count != 0 {
		t.Fatalf("queued OK status during recovery poll = %+v", events[:count])
	}
	rxObs := pcanReceiveObservation{hasFrame: true, frame: gocan.Frame{ID: 0x100}}
	state.observe(now.Add(2*statusPollInterval+5*time.Millisecond), rxObs)
	if state.nextDeadline().IsZero() {
		t.Fatal("queued status/error or duplicate 0x08 cancelled post-bus-off poll")
	}
	// Counted passive during post-bus-off polling must not flap Warning<->Passive
	// under repeated live 0x08 (BUSHEAVY) polls and repeated passive error frames.
	if events, count = state.observe(now.Add(2*statusPollInterval+6*time.Millisecond), passiveObs); count != 2 || events[1].ControllerState != gocan.ControllerPassive {
		t.Fatalf("passive during post-bus-off poll = %+v", events[:count])
	}
	for step := range 3 {
		pollAt := now.Add(time.Duration(3+step) * statusPollInterval)
		if polled, pcount, err = state.pollStatus(pollAt, pcanStatusBusWarning, 1, true); err != nil || pcount != 0 || state.nextDeadline().IsZero() {
			t.Fatalf("poll 0x08 while passive step %d = %+v, err=%v, next=%v", step, polled[:pcount], err, state.nextDeadline())
		}
		if events, count = state.observe(pollAt.Add(time.Millisecond), passiveObs); count != 1 || events[0].Kind != gocan.EventErrorFrame {
			t.Fatalf("repeated passive after poll 0x08 step %d = %+v", step, events[:count])
		}
	}
	clearStart := now.Add(6 * statusPollInterval)
	if polled, pcount, err = state.pollStatus(clearStart, pcanStatusOK, 1, true); err != nil || pcount != 0 || state.nextDeadline().IsZero() {
		t.Fatalf("first poll 0x00 = %+v, err=%v, next=%v", polled[:pcount], err, state.nextDeadline())
	}
	// Undrained queue at the hold boundary must defer publishing Active so
	// queued faults can invalidate the pending inferred recovery first.
	if polled, pcount, err = state.pollStatus(clearStart.Add(degradedClearHold), pcanStatusOK, 1, false); err != nil || pcount != 0 || state.nextDeadline().IsZero() {
		t.Fatalf("undrained poll 0x00 at clear hold = %+v, err=%v, next=%v", polled[:pcount], err, state.nextDeadline())
	}
	polled, pcount, err = state.pollStatus(clearStart.Add(degradedClearHold+statusPollInterval), pcanStatusOK, 1, true)
	if err != nil || pcount != 1 || polled[0].ControllerState != gocan.ControllerActive || !state.nextDeadline().IsZero() {
		t.Fatalf("poll 0x00 at clear hold = %+v, err=%v, next=%v", polled[:pcount], err, state.nextDeadline())
	}

	// 2. Non-bus-off overrun on direct bus-off recovery releases the write gate,
	// defers Active while the receive queue is undrained, then reports overrun +
	// Active once drained and resets pollOverrun for later recoveries.
	tBusOff := clearStart.Add(2 * time.Second)
	_, _ = state.observeSendBusOff(tBusOff, 1)
	polled, pcount, err = state.pollStatus(tBusOff.Add(statusPollInterval), pcanStatusOK, 1, false)
	if err != nil || state.busOff() || pcount != 0 || state.nextDeadline().IsZero() {
		t.Fatalf("undrained direct bus-off poll 0x00 = %+v, err=%v, busOff=%v, next=%v", polled[:pcount], err, state.busOff(), state.nextDeadline())
	}
	polled, pcount, err = state.pollStatus(tBusOff.Add(2*statusPollInterval), pcanStatusQueueOverrun, 1, true)
	if err != nil || state.busOff() || pcount != 2 ||
		polled[0].Kind != gocan.EventReceiveOverrun || polled[1].ControllerState != gocan.ControllerActive {
		t.Fatalf("poll QOVERRUN = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
	}
	state.observe(tBusOff.Add(3*statusPollInterval), rxObs)
	_, _ = state.observeSendBusOff(tBusOff.Add(4*statusPollInterval), 1)
	polled, pcount, err = state.pollStatus(tBusOff.Add(5*statusPollInterval), pcanStatusQueueOverrun, 1, true)
	if err != nil || state.busOff() || pcount != 2 ||
		polled[0].Kind != gocan.EventReceiveOverrun || polled[1].ControllerState != gocan.ControllerActive {
		t.Fatalf("second recovery poll QOVERRUN = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
	}

	// 3. Bench stub -> OBC RX-only -> overrun -> spurious bus-off -> clean recovery:
	// - Entry lag and post-first-status GetStatus 0x0 windows (~1.05 s) must not
	//   emit Active while periodic 0x08 status frames keep arriving.
	// - Clean RX frames while TX-degraded (TEC=127 Warning) must never infer Active
	//   or upgrade Warning back to Passive on 0x08 status frames.
	// - Receive overrun (0x02) clearing GetStatus to 0x0 for ~1.05 s must not emit Active.
	// - Spurious BUSOFF (0x10) alongside a TEC=95 error frame stays bus-off until
	//   live GetStatus clears BUSOFF.
	t0 := tBusOff.Add(2 * time.Second)
	tec128Obs, err := decodePCANErrorFrame([]byte{0, 0, 0, 128}, pcanStatusOK, 1)
	if err != nil {
		t.Fatal(err)
	}
	events, count = state.observe(t0, tec128Obs)
	if count != 2 || events[1].ControllerState != gocan.ControllerPassive || state.nextDeadline().IsZero() {
		t.Fatalf("enter passive = %+v, next=%v", events[:count], state.nextDeadline())
	}
	// Entry lag: GetStatus reads 0x0 for 950 ms until the first 0x08 status frame.
	if polled, pcount, err = state.pollStatus(t0.Add(statusPollInterval), pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("entry-lag poll 0x00 = %+v, err=%v", polled[:pcount], err)
	}
	if polled, pcount, err = state.pollStatus(t0.Add(950*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("entry-lag poll 0x00 at 950ms = %+v, err=%v", polled[:pcount], err)
	}
	if events, count = state.observe(t0.Add(time.Second), heavyObs); count != 0 {
		t.Fatalf("first BUSHEAVY downgraded passive: %+v", events[:count])
	}
	// Post-first-status drop: GetStatus reads 0x8 then drops to 0x0 for ~1.02 s
	// until the next ~1.05 s status frame.
	if polled, pcount, err = state.pollStatus(t0.Add(1020*time.Millisecond), pcanStatusBusWarning, 1, true); err != nil || pcount != 0 {
		t.Fatalf("post-status poll 0x08 = %+v, err=%v", polled[:pcount], err)
	}
	if polled, pcount, err = state.pollStatus(t0.Add(1040*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("post-status drop poll 0x00 = %+v, err=%v", polled[:pcount], err)
	}
	if polled, pcount, err = state.pollStatus(t0.Add(2040*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("post-status drop poll 0x00 at +1.0s = %+v, err=%v", polled[:pcount], err)
	}
	if events, count = state.observe(t0.Add(2050*time.Millisecond), heavyObs); count != 0 {
		t.Fatalf("second BUSHEAVY downgraded passive: %+v", events[:count])
	}
	// Plug into healthy bus with no TX: queued frame drops TEC 128->127 (Warning);
	// clean RX + 1.05 s BUSHEAVY status frames must keep steady Warning with no flicker.
	tec127Obs, err := decodePCANErrorFrame([]byte{0, 0, 0, 127}, pcanStatusOK, 1)
	if err != nil {
		t.Fatal(err)
	}
	if events, count = state.observe(t0.Add(3*time.Second), tec127Obs); count != 2 || events[1].ControllerState != gocan.ControllerWarning {
		t.Fatalf("TEC=127 transition = %+v", events[:count])
	}
	for sec := range 4 {
		base := t0.Add(time.Duration(4+sec) * time.Second)
		if events, count = state.observe(base, rxObs); count != 0 {
			t.Fatalf("clean RX while TEC=127 emitted %+v", events[:count])
		}
		if events, count = state.observe(base.Add(50*time.Millisecond), heavyObs); count != 0 {
			t.Fatalf("BUSHEAVY while TEC=127 emitted %+v", events[:count])
		}
		if polled, pcount, err = state.pollStatus(base.Add(100*time.Millisecond), pcanStatusBusWarning, 1, true); err != nil || pcount != 0 {
			t.Fatalf("poll 0x08 while TEC=127 = %+v, err=%v", polled[:pcount], err)
		}
	}
	// Overrun from CAN_Read (0x02) resets any in-progress clear window and
	// clears GetStatus to 0x0 until the next ~1.05 s status frame; pollStatus(0x02)
	// also resets the clear window.
	tOvr := t0.Add(9 * time.Second)
	if polled, pcount, err = state.pollStatus(tOvr.Add(-400*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("pre-overrun poll 0x00 = %+v, err=%v", polled[:pcount], err)
	}
	overrunObs, err := decodePCANStatus(pcanStatusOverrun, 1)
	if err != nil {
		t.Fatal(err)
	}
	if events, count = state.observe(tOvr, overrunObs); count != 1 || events[0].Kind != gocan.EventReceiveOverrun {
		t.Fatalf("overrun observation = %+v", events[:count])
	}
	if polled, pcount, err = state.pollStatus(tOvr.Add(20*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("post-overrun poll 0x00 = %+v, err=%v", polled[:pcount], err)
	}
	if polled, pcount, err = state.pollStatus(tOvr.Add(1040*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("post-overrun poll 0x00 at +1.02s = %+v, err=%v", polled[:pcount], err)
	}
	if events, count = state.observe(tOvr.Add(1050*time.Millisecond), heavyObs); count != 0 {
		t.Fatalf("post-overrun BUSHEAVY = %+v", events[:count])
	}
	if polled, pcount, err = state.pollStatus(tOvr.Add(1100*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("poll 0x00 before poll overrun = %+v, err=%v", polled[:pcount], err)
	}
	if polled, pcount, err = state.pollStatus(tOvr.Add(1500*time.Millisecond), pcanStatusOverrun, 1, true); err != nil || pcount != 1 || polled[0].Kind != gocan.EventReceiveOverrun {
		t.Fatalf("poll overrun while degraded = %+v, err=%v", polled[:pcount], err)
	}
	if polled, pcount, err = state.pollStatus(tOvr.Add(2500*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("poll 0x00 after poll overrun reset = %+v, err=%v", polled[:pcount], err)
	}
	// Spurious BUSOFF (0x10) paired with TEC=95 error frame stays bus-off until
	// live GetStatus clears BUSOFF.
	tSpurious := tOvr.Add(3 * time.Second)
	busOffObs, _ := decodePCANStatus(pcanStatusBusOff, 1)
	if events, count = state.observe(tSpurious, busOffObs); count != 1 || events[0].ControllerState != gocan.ControllerBusOff || !state.busOff() {
		t.Fatalf("spurious bus-off = %+v, busOff=%v", events[:count], state.busOff())
	}
	tec95Obs, err := decodePCANErrorFrame([]byte{0, 0, 0, 95}, pcanStatusOK, 1)
	if err != nil {
		t.Fatal(err)
	}
	if events, count = state.observe(tSpurious.Add(time.Millisecond), tec95Obs); count != 1 || events[0].Kind != gocan.EventErrorFrame || !state.busOff() {
		t.Fatalf("TEC=95 during spurious bus-off = %+v, busOff=%v", events[:count], state.busOff())
	}
	if polled, pcount, err = state.pollStatus(tSpurious.Add(time.Second), pcanStatusOK, 1, true); err != nil || pcount != 1 || polled[0].ControllerState != gocan.ControllerActive || state.busOff() || !state.nextDeadline().IsZero() {
		t.Fatalf("spurious bus-off recovery = %+v, err=%v, busOff=%v, next=%v", polled[:pcount], err, state.busOff(), state.nextDeadline())
	}

	// 4. Clean recovery outside bus-off:
	// - Counted error frame below 96 (TEC=95) recovers immediately to Active
	//   with counts and stops polling (preventing occasional low-counter errors
	//   from holding the channel degraded forever).
	// - Without a counted recovery frame, sustained pollStatus(0x00) for
	//   degradedClearHold infers Active and stops polling.
	tCounted := tSpurious.Add(2 * time.Second)
	if events, count = state.observe(tCounted, tec128Obs); count != 2 || events[1].ControllerState != gocan.ControllerPassive {
		t.Fatalf("re-enter passive for counted recovery = %+v", events[:count])
	}
	if events, count = state.observe(tCounted.Add(10*time.Millisecond), tec95Obs); count != 2 ||
		events[1].ControllerState != gocan.ControllerActive || !events[1].ErrorCountsKnown ||
		events[1].TXErrorCount != 95 || !state.nextDeadline().IsZero() {
		t.Fatalf("counted TEC=95 recovery = %+v, next=%v", events[:count], state.nextDeadline())
	}

	tInferred := tCounted.Add(time.Second)
	if events, count = state.observe(tInferred, tec128Obs); count != 2 || events[1].ControllerState != gocan.ControllerPassive {
		t.Fatalf("re-enter passive for inferred recovery = %+v", events[:count])
	}
	lightObs, _ := decodePCANStatus(pcanStatusBusLight, 1)
	if events, count = state.observe(tInferred.Add(time.Millisecond), lightObs); count != 1 || events[0].ControllerState != gocan.ControllerWarning {
		t.Fatalf("BUSLIGHT after passive = %+v", events[:count])
	}
	tClear := tInferred.Add(statusPollInterval)
	if polled, pcount, err = state.pollStatus(tClear, pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("inferred recovery start poll = %+v, err=%v", polled[:pcount], err)
	}
	if polled, pcount, err = state.pollStatus(tClear.Add(degradedClearHold-statusPollInterval), pcanStatusOK, 1, true); err != nil || pcount != 0 {
		t.Fatalf("inferred recovery early poll = %+v, err=%v", polled[:pcount], err)
	}
	if polled, pcount, err = state.pollStatus(tClear.Add(degradedClearHold), pcanStatusOK, 1, true); err != nil || pcount != 1 || polled[0].ControllerState != gocan.ControllerActive || !state.nextDeadline().IsZero() {
		t.Fatalf("inferred recovery final poll = %+v, err=%v, next=%v", polled[:pcount], err, state.nextDeadline())
	}
}
