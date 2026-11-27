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
	heavyObs, _ := decodePCANStatus(pcanStatusBusWarning, 1)
	lightObs, _ := decodePCANStatus(pcanStatusBusLight, 1)
	okObs, _ := decodePCANStatus(pcanStatusOK, 1)
	busOffObs, _ := decodePCANStatus(pcanStatusBusOff, 1)
	overrunObs, _ := decodePCANStatus(pcanStatusOverrun, 1)
	qOverrunObs, _ := decodePCANStatus(pcanStatusQueueOverrun, 1)
	rxObs := pcanReceiveObservation{hasFrame: true, frame: gocan.Frame{ID: 0x100}}

	errorObs := func(rec, tec byte) pcanReceiveObservation {
		t.Helper()
		obs, err := decodePCANErrorFrame([]byte{0, 0, rec, tec}, pcanStatusOK, 1)
		if err != nil {
			t.Fatal(err)
		}
		return obs
	}
	passiveObs := errorObs(129, 0)
	tec128Obs := errorObs(0, 128)
	tec127Obs := errorObs(0, 127)
	tec95Obs := errorObs(0, 95)

	t.Run("bus-off recovery", func(t *testing.T) {
		var state pcanState

		// Send hits BUSOFF: write gate latches, queued passive error cannot release it,
		// live 0x14 stays bus-off, and live 0x08 releases writes into Warning.
		event, ok := state.observeSendBusOff(now, 1)
		if !ok || event.ControllerState != gocan.ControllerBusOff || !state.busOff() {
			t.Fatalf("send bus-off = %+v, ok=%v, busOff=%v", event, ok, state.busOff())
		}
		if events, count := state.observe(now.Add(time.Millisecond), passiveObs); count != 1 || events[0].Kind != gocan.EventErrorFrame || !state.busOff() {
			t.Fatalf("queued error during bus-off = %+v, busOff=%v", events[:count], state.busOff())
		}
		if polled, pcount, err := state.pollStatus(now.Add(statusPollInterval), pcanStatusBusOff|pcanStatusBusLight, 1, true); err != nil || pcount != 0 || !state.busOff() {
			t.Fatalf("poll 0x14 = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
		}
		if polled, pcount, err := state.pollStatus(now.Add(2*statusPollInterval), pcanStatusBusWarning, 1, true); err != nil || pcount != 1 || polled[0].ControllerState != gocan.ControllerWarning || state.busOff() {
			t.Fatalf("poll 0x08 = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
		}

		// Duplicate 0x08, stale low-counter error frames, queued OK, and clean RX
		// must neither cancel post-bus-off polling nor report Active prematurely.
		state.observe(now.Add(2*statusPollInterval+time.Millisecond), heavyObs)
		state.observe(now.Add(2*statusPollInterval+2*time.Millisecond), errorObs(0, 100))
		if events, count := state.observe(now.Add(2*statusPollInterval+3*time.Millisecond), errorObs(0, 8)); count != 1 || events[0].Kind != gocan.EventErrorFrame {
			t.Fatalf("stale low-counter error during post-bus-off poll = %+v", events[:count])
		}
		if events, count := state.observe(now.Add(2*statusPollInterval+4*time.Millisecond), okObs); count != 0 {
			t.Fatalf("queued OK status during recovery poll = %+v", events[:count])
		}
		state.observe(now.Add(2*statusPollInterval+5*time.Millisecond), rxObs)
		if state.nextDeadline().IsZero() {
			t.Fatal("queued status/error or duplicate 0x08 cancelled post-bus-off poll")
		}

		// Counted passive must not flap Warning<->Passive under repeated 0x08 polls.
		if events, count := state.observe(now.Add(2*statusPollInterval+6*time.Millisecond), passiveObs); count != 2 || events[1].ControllerState != gocan.ControllerPassive {
			t.Fatalf("passive during post-bus-off poll = %+v", events[:count])
		}
		for step := range 3 {
			pollAt := now.Add(time.Duration(3+step) * statusPollInterval)
			if polled, pcount, err := state.pollStatus(pollAt, pcanStatusBusWarning, 1, true); err != nil || pcount != 0 || state.nextDeadline().IsZero() {
				t.Fatalf("poll 0x08 while passive step %d = %+v, err=%v, next=%v", step, polled[:pcount], err, state.nextDeadline())
			}
			if events, count := state.observe(pollAt.Add(time.Millisecond), passiveObs); count != 1 || events[0].Kind != gocan.EventErrorFrame {
				t.Fatalf("repeated passive after poll 0x08 step %d = %+v", step, events[:count])
			}
		}

		// Clear poll must hold for degradedClearHold and wait for an empty queue.
		clearStart := now.Add(6 * statusPollInterval)
		if polled, pcount, err := state.pollStatus(clearStart, pcanStatusOK, 1, true); err != nil || pcount != 0 || state.nextDeadline().IsZero() {
			t.Fatalf("first poll 0x00 = %+v, err=%v, next=%v", polled[:pcount], err, state.nextDeadline())
		}
		if polled, pcount, err := state.pollStatus(clearStart.Add(degradedClearHold), pcanStatusOK, 1, false); err != nil || pcount != 0 || state.nextDeadline().IsZero() {
			t.Fatalf("undrained poll 0x00 at clear hold = %+v, err=%v, next=%v", polled[:pcount], err, state.nextDeadline())
		}
		if polled, pcount, err := state.pollStatus(clearStart.Add(degradedClearHold+statusPollInterval), pcanStatusOK, 1, true); err != nil || pcount != 1 || polled[0].ControllerState != gocan.ControllerActive || !state.nextDeadline().IsZero() {
			t.Fatalf("drained poll 0x00 at clear hold = %+v, err=%v, next=%v", polled[:pcount], err, state.nextDeadline())
		}

		// Direct bus-off recovery releases writes immediately, defers Active while
		// undrained without arming classic overrun suppression, then reports
		// QOVERRUN + Active and resets pollOverrun for subsequent recoveries.
		state.classic = true
		tBusOff := clearStart.Add(2 * time.Second)
		_, _ = state.observeSendBusOff(tBusOff, 1)
		if polled, pcount, err := state.pollStatus(tBusOff.Add(statusPollInterval), pcanStatusOK, 1, false); err != nil || state.busOff() || pcount != 0 || state.nextDeadline().IsZero() {
			t.Fatalf("undrained direct bus-off poll 0x00 = %+v, err=%v, busOff=%v, next=%v", polled[:pcount], err, state.busOff(), state.nextDeadline())
		}
		state.observeSend(tBusOff.Add(statusPollInterval))
		if events, count := state.observe(tBusOff.Add(statusPollInterval+time.Millisecond), overrunObs); count != 1 || events[0].Kind != gocan.EventReceiveOverrun {
			t.Fatalf("bare 0x02 during deferred bus-off recovery = %+v", events[:count])
		}
		if polled, pcount, err := state.pollStatus(tBusOff.Add(2*statusPollInterval), pcanStatusQueueOverrun, 1, true); err != nil || state.busOff() || pcount != 2 ||
			polled[0].Kind != gocan.EventReceiveOverrun || polled[1].ControllerState != gocan.ControllerActive {
			t.Fatalf("poll QOVERRUN = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
		}
		_, _ = state.observeSendBusOff(tBusOff.Add(3*statusPollInterval), 1)
		if polled, pcount, err := state.pollStatus(tBusOff.Add(4*statusPollInterval), pcanStatusQueueOverrun, 1, true); err != nil || state.busOff() || pcount != 2 ||
			polled[0].Kind != gocan.EventReceiveOverrun || polled[1].ControllerState != gocan.ControllerActive {
			t.Fatalf("second recovery poll QOVERRUN = %+v, err=%v, busOff=%v", polled[:pcount], err, state.busOff())
		}
	})

	t.Run("degraded stability and recovery", func(t *testing.T) {
		var state pcanState

		// Entry lag: GetStatus reads 0x0 for ~1 s until the first 0x08 status frame;
		// BUSHEAVY resets the hold and preserves Passive.
		if events, count := state.observe(now, tec128Obs); count != 2 || events[1].ControllerState != gocan.ControllerPassive || state.nextDeadline().IsZero() {
			t.Fatalf("enter passive = %+v, next=%v", events[:count], state.nextDeadline())
		}
		if polled, pcount, err := state.pollStatus(now.Add(statusPollInterval), pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("entry-lag poll 0x00 = %+v, err=%v", polled[:pcount], err)
		}
		if polled, pcount, err := state.pollStatus(now.Add(1050*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("entry-lag poll 0x00 at 1.05s = %+v, err=%v", polled[:pcount], err)
		}
		if events, count := state.observe(now.Add(1060*time.Millisecond), heavyObs); count != 0 {
			t.Fatalf("BUSHEAVY downgraded passive: %+v", events[:count])
		}

		// Clean RX while TEC=127 (Warning) + periodic 0x08 status frames must keep
		// steady Warning with no flicker to Active or upgrade back to Passive.
		if events, count := state.observe(now.Add(2*time.Second), tec127Obs); count != 2 || events[1].ControllerState != gocan.ControllerWarning {
			t.Fatalf("TEC=127 transition = %+v", events[:count])
		}
		for sec := range 3 {
			base := now.Add(time.Duration(3+sec) * time.Second)
			if events, count := state.observe(base, rxObs); count != 0 {
				t.Fatalf("clean RX while TEC=127 emitted %+v", events[:count])
			}
			if events, count := state.observe(base.Add(50*time.Millisecond), heavyObs); count != 0 {
				t.Fatalf("BUSHEAVY while TEC=127 emitted %+v", events[:count])
			}
			if polled, pcount, err := state.pollStatus(base.Add(100*time.Millisecond), pcanStatusBusWarning, 1, true); err != nil || pcount != 0 {
				t.Fatalf("poll 0x08 while TEC=127 = %+v, err=%v", polled[:pcount], err)
			}
		}

		// Spurious BUSOFF entered from Warning resets the bus-off poll spacing
		// to a full statusPollInterval and stays bus-off across a paired TEC=95
		// error frame until live GetStatus clears.
		tSpurious := now.Add(7 * time.Second)
		if events, count := state.observe(tSpurious, busOffObs); count != 1 || events[0].ControllerState != gocan.ControllerBusOff || !state.busOff() {
			t.Fatalf("spurious bus-off = %+v, busOff=%v", events[:count], state.busOff())
		}
		if got, want := state.nextDeadline(), tSpurious.Add(statusPollInterval); !got.Equal(want) {
			t.Fatalf("bus-off entry deadline = %v, want %v", got, want)
		}
		if events, count := state.observe(tSpurious.Add(time.Millisecond), tec95Obs); count != 1 || events[0].Kind != gocan.EventErrorFrame || !state.busOff() {
			t.Fatalf("TEC=95 during spurious bus-off = %+v, busOff=%v", events[:count], state.busOff())
		}
		if polled, pcount, err := state.pollStatus(tSpurious.Add(time.Second), pcanStatusOK, 1, true); err != nil || pcount != 1 || polled[0].ControllerState != gocan.ControllerActive || state.busOff() || !state.nextDeadline().IsZero() {
			t.Fatalf("spurious bus-off recovery = %+v, err=%v, busOff=%v, next=%v", polled[:pcount], err, state.busOff(), state.nextDeadline())
		}

		// Counted error frame below 96 outside bus-off recovers immediately with counts.
		tCounted := tSpurious.Add(2 * time.Second)
		state.observe(tCounted, tec128Obs)
		if events, count := state.observe(tCounted.Add(10*time.Millisecond), tec95Obs); count != 2 ||
			events[1].ControllerState != gocan.ControllerActive || !events[1].ErrorCountsKnown ||
			events[1].TXErrorCount != 95 || !state.nextDeadline().IsZero() {
			t.Fatalf("counted TEC=95 recovery = %+v, next=%v", events[:count], state.nextDeadline())
		}

		// BUSLIGHT steps Passive -> Warning; without counted recovery or RX frames,
		// sustained GetStatus 0x0 for degradedClearHold infers Active.
		tInferred := tCounted.Add(time.Second)
		state.observe(tInferred, tec128Obs)
		if events, count := state.observe(tInferred.Add(time.Millisecond), lightObs); count != 1 || events[0].ControllerState != gocan.ControllerWarning {
			t.Fatalf("BUSLIGHT after passive = %+v", events[:count])
		}
		tClear := tInferred.Add(statusPollInterval)
		if polled, pcount, err := state.pollStatus(tClear, pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("inferred recovery start poll = %+v, err=%v", polled[:pcount], err)
		}
		if polled, pcount, err := state.pollStatus(tClear.Add(degradedClearHold-statusPollInterval), pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("inferred recovery early poll = %+v, err=%v", polled[:pcount], err)
		}
		if polled, pcount, err := state.pollStatus(tClear.Add(degradedClearHold), pcanStatusOK, 1, true); err != nil || pcount != 1 || polled[0].ControllerState != gocan.ControllerActive || !state.nextDeadline().IsZero() {
			t.Fatalf("inferred recovery final poll = %+v, err=%v, next=%v", polled[:pcount], err, state.nextDeadline())
		}
	})

	t.Run("classic drain overrun artifact", func(t *testing.T) {
		state := pcanState{classic: true}
		state.observe(now, tec127Obs)

		// Before any degraded TX, bare 0x02 is reported and resets the clear hold.
		if polled, pcount, err := state.pollStatus(now.Add(statusPollInterval), pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("pre-overrun poll 0x00 = %+v, err=%v", polled[:pcount], err)
		}
		tPreTX := now.Add(500 * time.Millisecond)
		if events, count := state.observe(tPreTX, overrunObs); count != 1 || events[0].Kind != gocan.EventReceiveOverrun {
			t.Fatalf("pre-TX bare 0x02 = %+v", events[:count])
		}
		if polled, pcount, err := state.pollStatus(tPreTX.Add(statusPollInterval), pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("post-pre-TX overrun poll start = %+v, err=%v", polled[:pcount], err)
		}
		if polled, pcount, err := state.pollStatus(tPreTX.Add(1100*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("pre-TX overrun did not reset clear hold: %+v, err=%v", polled[:pcount], err)
		}

		// Within degradedSendOverrunWindow of a degraded TX on classic hardware,
		// bare 0x02 is suppressed in both observe and pollStatus while still
		// resetting the clear hold; 0x40 (QOVERRUN) stays reported.
		tObsOvr := tPreTX.Add(1200 * time.Millisecond)
		state.observeSend(tObsOvr.Add(-16 * time.Millisecond))
		if events, count := state.observe(tObsOvr, overrunObs); count != 0 {
			t.Fatalf("classic drain observe bare 0x02 not suppressed: %+v", events[:count])
		}
		if polled, pcount, err := state.pollStatus(tObsOvr.Add(statusPollInterval), pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("post-suppressed-observe poll start = %+v, err=%v", polled[:pcount], err)
		}
		if polled, pcount, err := state.pollStatus(tObsOvr.Add(1100*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("suppressed observe overrun did not reset clear hold: %+v, err=%v", polled[:pcount], err)
		}
		tPollOvr := tObsOvr.Add(1200 * time.Millisecond)
		state.observeSend(tPollOvr.Add(-16 * time.Millisecond))
		if polled, pcount, err := state.pollStatus(tPollOvr, pcanStatusOverrun, 1, true); err != nil || pcount != 0 {
			t.Fatalf("classic drain poll bare 0x02 not suppressed: %+v, err=%v", polled[:pcount], err)
		}
		if polled, pcount, err := state.pollStatus(tPollOvr.Add(statusPollInterval), pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("post-suppressed-poll poll start = %+v, err=%v", polled[:pcount], err)
		}
		if polled, pcount, err := state.pollStatus(tPollOvr.Add(1100*time.Millisecond), pcanStatusOK, 1, true); err != nil || pcount != 0 {
			t.Fatalf("suppressed poll overrun did not reset clear hold: %+v, err=%v", polled[:pcount], err)
		}
		// After degradedSendOverrunWindow elapses without another TX, bare 0x02
		// during a receive-only degraded period is reported again.
		if events, count := state.observe(tPollOvr.Add(1120*time.Millisecond), overrunObs); count != 1 || events[0].Kind != gocan.EventReceiveOverrun {
			t.Fatalf("receive-only bare 0x02 after TX window expired = %+v", events[:count])
		}
		state.observeSend(tPollOvr.Add(1140 * time.Millisecond))
		if events, count := state.observe(tPollOvr.Add(1150*time.Millisecond), qOverrunObs); count != 1 || events[0].Kind != gocan.EventReceiveOverrun {
			t.Fatalf("QOVERRUN during classic drain = %+v", events[:count])
		}

		// Recovery re-enables bare 0x02 reporting even within the TX window;
		// non-classic (FD-capable) channels never suppress bare 0x02.
		state.observe(tPollOvr.Add(1160*time.Millisecond), tec95Obs)
		if events, count := state.observe(tPollOvr.Add(1170*time.Millisecond), overrunObs); count != 1 || events[0].Kind != gocan.EventReceiveOverrun {
			t.Fatalf("bare 0x02 after recovery = %+v", events[:count])
		}
		var fdState pcanState
		fdState.observe(now, tec128Obs)
		fdState.observeSend(now.Add(time.Millisecond))
		if events, count := fdState.observe(now.Add(10*time.Millisecond), overrunObs); count != 1 || events[0].Kind != gocan.EventReceiveOverrun {
			t.Fatalf("FD-capable bare 0x02 during degraded drain = %+v", events[:count])
		}
	})
}
