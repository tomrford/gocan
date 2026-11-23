package gocan

import (
	"context"
	"encoding/binary"
	"errors"
	"iter"
	"runtime"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

const (
	testBus0 BusID = iota + 1
	testBus1
)

var captureTestBase = time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

func testDataEvent(t *testing.T, bus BusID, id uint32, flags FrameFlags, data []byte, seq int, direction Direction) FrameEvent {
	t.Helper()
	frame, err := NewFrame(id, data, flags)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	return FrameEvent{
		Bus:       bus,
		Timestamp: captureTestBase.Add(time.Duration(seq) * time.Millisecond),
		Direction: direction,
		Frame:     frame,
	}
}

func eventsEqual(a, b FrameEvent) bool {
	return a.Bus == b.Bus &&
		a.Direction == b.Direction &&
		a.Timestamp.Equal(b.Timestamp) &&
		a.Frame == b.Frame
}

func requireEvents(t *testing.T, label string, got, want []FrameEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d events, want %d", label, len(got), len(want))
	}
	for i := range want {
		if !eventsEqual(got[i], want[i]) {
			t.Fatalf("%s: event %d = %+v, want %+v", label, i, got[i], want[i])
		}
	}
}

func requireBusEvents(t *testing.T, label string, got, want []Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d events, want %d", label, len(got), len(want))
	}
	for i := range want {
		if got[i].Bus != want[i].Bus ||
			!got[i].Timestamp.Equal(want[i].Timestamp) ||
			got[i].Kind != want[i].Kind ||
			got[i].ControllerState != want[i].ControllerState ||
			got[i].TXErrorCount != want[i].TXErrorCount ||
			got[i].RXErrorCount != want[i].RXErrorCount {
			t.Fatalf("%s: event %d = %+v, want %+v", label, i, got[i], want[i])
		}
	}
}

type captureWriterProbe struct {
	failAt     int
	failure    error
	calls      int
	frames     []uint32
	events     []EventKind
	timestamps []time.Time
}

func (writer *captureWriterProbe) WriteFrame(event FrameEvent) error {
	if err := writer.next(); err != nil {
		return err
	}
	writer.frames = append(writer.frames, event.Frame.ID)
	writer.timestamps = append(writer.timestamps, event.Timestamp)
	return nil
}

func (writer *captureWriterProbe) WriteEvent(event Event) error {
	if err := writer.next(); err != nil {
		return err
	}
	writer.events = append(writer.events, event.Kind)
	writer.timestamps = append(writer.timestamps, event.Timestamp)
	return nil
}

func (writer *captureWriterProbe) next() error {
	call := writer.calls
	writer.calls++
	if call == writer.failAt {
		return writer.failure
	}
	return nil
}

func TestCaptureLiveClock(t *testing.T) {
	start := time.Now()
	capture := NewCapture()
	if err := capture.RecordFrame(FrameEvent{Bus: 1, Direction: DirectionReceive, Frame: Frame{ID: MaxExtendedID + 1}}); err == nil {
		t.Fatal("accepted invalid live frame")
	}
	if err := capture.RecordEvent(Event{Bus: 1, Kind: EventControllerState}); err == nil {
		t.Fatal("accepted invalid live controller state")
	}
	if capture.Len() != 0 {
		t.Fatal("invalid records changed capture")
	}

	for range 2 {
		var writers sync.WaitGroup
		for bus := BusID(1); bus <= 4; bus++ {
			writers.Go(func() {
				for range 1024 {
					if err := capture.RecordFrame(FrameEvent{Bus: bus, Timestamp: captureTestBase, Direction: DirectionReceive}); err != nil {
						t.Error(err)
						return
					}
					if err := capture.RecordEvent(Event{Bus: bus, Kind: EventErrorFrame}); err != nil {
						t.Error(err)
						return
					}
				}
			})
		}
		writers.Wait()
		end := time.Now()
		writer := &captureWriterProbe{}
		if _, err := capture.WriteRecords(Range{}, writer); err != nil {
			t.Fatal(err)
		}
		if len(writer.timestamps) != 8192 {
			t.Fatalf("record count = %d, want 8192", len(writer.timestamps))
		}
		for i, timestamp := range writer.timestamps {
			if timestamp.Before(start) || timestamp.After(end) {
				t.Fatalf("record %d timestamp %s outside ordered interval [%s, %s]", i, timestamp, start, end)
			}
			start = timestamp
		}
		capture.Clear()
	}
}

func newTestCapture(recordCapacity, payloadCapacity int) *Capture {
	return newCapture(recordCapacity, payloadCapacity)
}

func collectAllFrames(t *testing.T, capture *Capture) []FrameEvent {
	t.Helper()
	seq, _, err := capture.Frames(Range{})
	if err != nil {
		t.Fatalf("Frames(Range{}): %v", err)
	}
	return slices.Collect(seq)
}

func collectAllEvents(t *testing.T, capture *Capture) []Event {
	t.Helper()
	seq, _, err := capture.Events(Range{})
	if err != nil {
		t.Fatalf("Events(Range{}): %v", err)
	}
	return slices.Collect(seq)
}

func collectAllSeries(t *testing.T, capture *Capture, key FrameKey) []FrameEvent {
	t.Helper()
	seq, _, err := capture.Series(key, Range{})
	if err != nil {
		t.Fatalf("Series(%+v, Range{}): %v", key, err)
	}
	return slices.Collect(seq)
}

func collectAllBusEvents(t *testing.T, capture *Capture, bus BusID) []Event {
	t.Helper()
	seq, _, err := capture.BusEvents(bus, Range{})
	if err != nil {
		t.Fatalf("BusEvents(%d, Range{}): %v", bus, err)
	}
	return slices.Collect(seq)
}

func TestCaptureLifecycle(t *testing.T) {
	capture := NewCapture()

	remote, err := NewRemoteFrame(0x321, 4, false)
	if err != nil {
		t.Fatalf("NewRemoteFrame: %v", err)
	}
	events := []FrameEvent{
		testDataEvent(t, testBus0, 0x100, 0, []byte{1, 2, 3}, 0, DirectionReceive),
		testDataEvent(t, testBus1, 0x100, 0, []byte{4, 5}, 1, DirectionReceive),
		testDataEvent(t, testBus0, 0x100, 0, []byte{6}, 2, DirectionTransmit),
		testDataEvent(t, testBus0, 0x1abcde, FrameExtended, []byte{7, 8}, 3, DirectionReceive),
		testDataEvent(t, testBus0, 0x200, FrameFD|FrameBitRateSwitch, make([]byte, 64), 4, DirectionReceive),
		{
			Bus:       testBus1,
			Timestamp: captureTestBase.Add(5 * time.Millisecond),
			Direction: DirectionReceive,
			Frame:     remote,
		},
	}
	for i, event := range events {
		if err := capture.Append(event); err != nil {
			t.Fatalf("Append event %d: %v", i, err)
		}
	}

	if got := capture.Len(); got != len(events) {
		t.Fatalf("Len() = %d, want %d", got, len(events))
	}
	requireEvents(t, "Frames", collectAllFrames(t, capture), events)

	requireEvents(t, "Series bus0/0x100 RX",
		collectAllSeries(t, capture, FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}),
		[]FrameEvent{events[0]})
	requireEvents(t, "Series bus0/0x100 TX",
		collectAllSeries(t, capture, FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionTransmit}),
		[]FrameEvent{events[2]})
	requireEvents(t, "Series bus1/0x100",
		collectAllSeries(t, capture, FrameKey{Bus: testBus1, ID: 0x100, Direction: DirectionReceive}),
		[]FrameEvent{events[1]})
	requireEvents(t, "Series extended",
		collectAllSeries(t, capture, FrameKey{Bus: testBus0, ID: 0x1abcde, Direction: DirectionReceive, Extended: true}),
		[]FrameEvent{events[3]})

	latest, ok := capture.Latest(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive})
	if !ok || !eventsEqual(latest, events[0]) {
		t.Fatalf("Latest RX = %+v (ok=%t), want %+v", latest, ok, events[0])
	}
	latest, ok = capture.Latest(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionTransmit})
	if !ok || !eventsEqual(latest, events[2]) {
		t.Fatalf("Latest TX = %+v (ok=%t), want %+v", latest, ok, events[2])
	}

	if err := capture.Append(FrameEvent{}); err == nil {
		t.Fatal("Append accepted an invalid event")
	}
	if got := capture.Len(); got != len(events) {
		t.Fatalf("Len() after rejected append = %d, want %d", got, len(events))
	}

	capture.Clear()
	if got := capture.Len(); got != 0 {
		t.Fatalf("Len() after Clear = %d, want 0", got)
	}
	if got := collectAllFrames(t, capture); len(got) != 0 {
		t.Fatalf("Frames after Clear returned %d events", len(got))
	}
	if _, ok := capture.Latest(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}); ok {
		t.Fatal("Latest found a key after Clear")
	}
	if got := collectAllSeries(t, capture, FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}); len(got) != 0 {
		t.Fatalf("Series after Clear returned %d events", len(got))
	}

	revived := testDataEvent(t, testBus0, 0x100, 0, []byte{9}, 6, DirectionReceive)
	if err := capture.Append(revived); err != nil {
		t.Fatalf("Append after Clear: %v", err)
	}
	requireEvents(t, "Series after Clear",
		collectAllSeries(t, capture, FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}),
		[]FrameEvent{revived})
	latest, ok = capture.Latest(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive})
	if !ok || !eventsEqual(latest, revived) {
		t.Fatalf("Latest after Clear = %+v (ok=%t), want %+v", latest, ok, revived)
	}
}

func TestCaptureMixedRecords(t *testing.T) {
	capture := newTestCapture(4, 24)
	frame0 := testDataEvent(t, testBus0, 0x100, 0, []byte{1}, 0, DirectionReceive)
	state0, err := NewControllerStateEvent(
		testBus0,
		captureTestBase.Add(time.Millisecond),
		ControllerWarning,
		12,
		34,
		true,
	)
	if err != nil {
		t.Fatalf("NewControllerStateEvent: %v", err)
	}
	frame1 := testDataEvent(t, testBus1, 0x200, 0, []byte{2}, 2, DirectionReceive)
	for _, appendRecord := range []func() error{
		func() error { return capture.Append(frame0) },
		func() error { return capture.AppendEvent(state0) },
		func() error { return capture.Append(frame1) },
	} {
		if err := appendRecord(); err != nil {
			t.Fatalf("append initial record: %v", err)
		}
	}
	cursor := capture.End()

	error1, err := NewErrorFrameEvent(testBus1, captureTestBase.Add(3*time.Millisecond))
	if err != nil {
		t.Fatalf("NewErrorFrameEvent: %v", err)
	}
	overrun0, err := NewReceiveOverrunEvent(testBus0, captureTestBase.Add(4*time.Millisecond))
	if err != nil {
		t.Fatalf("NewReceiveOverrunEvent: %v", err)
	}
	frame2 := testDataEvent(t, testBus0, 0x100, 0, []byte{3}, 5, DirectionTransmit)
	state1, err := NewControllerStateEvent(
		testBus1,
		captureTestBase.Add(6*time.Millisecond),
		ControllerActive,
		0,
		1,
		true,
	)
	if err != nil {
		t.Fatalf("NewControllerStateEvent: %v", err)
	}
	for _, appendRecord := range []func() error{
		func() error { return capture.AppendEvent(error1) },
		func() error { return capture.AppendEvent(overrun0) },
		func() error { return capture.Append(frame2) },
		func() error { return capture.AppendEvent(state1) },
	} {
		if err := appendRecord(); err != nil {
			t.Fatalf("append later record: %v", err)
		}
	}

	if got := capture.Len(); got != 7 {
		t.Fatalf("Len() = %d, want 7 records", got)
	}
	requireEvents(t, "all frames", collectAllFrames(t, capture), []FrameEvent{frame0, frame1, frame2})
	requireBusEvents(t, "all events", collectAllEvents(t, capture), []Event{state0, error1, overrun0, state1})
	requireBusEvents(t, "bus 0 events", collectAllBusEvents(t, capture, testBus0), []Event{state0, overrun0})
	requireBusEvents(t, "bus 1 events", collectAllBusEvents(t, capture, testBus1), []Event{error1, state1})

	frameSeq, frameCursor, err := capture.Frames(Since(cursor))
	if err != nil {
		t.Fatalf("Frames(Since): %v", err)
	}
	requireEvents(t, "later frames", slices.Collect(frameSeq), []FrameEvent{frame2})
	eventSeq, eventCursor, err := capture.Events(Since(cursor))
	if err != nil {
		t.Fatalf("Events(Since): %v", err)
	}
	requireBusEvents(t, "later events", slices.Collect(eventSeq), []Event{error1, overrun0, state1})
	bus0Seq, bus0Cursor, err := capture.BusEvents(testBus0, Since(cursor))
	if err != nil {
		t.Fatalf("BusEvents(Since) bus 0: %v", err)
	}
	requireBusEvents(t, "later bus 0 events", slices.Collect(bus0Seq), []Event{overrun0})
	bus1Seq, bus1Cursor, err := capture.BusEvents(testBus1, Since(cursor))
	if err != nil {
		t.Fatalf("BusEvents(Since) bus 1: %v", err)
	}
	requireBusEvents(t, "later bus 1 events", slices.Collect(bus1Seq), []Event{error1, state1})
	if frameCursor != eventCursor || frameCursor != bus0Cursor || frameCursor != bus1Cursor || frameCursor != capture.End() {
		t.Fatalf("Since cursors did not advance to the shared capture end")
	}

	invalid := Event{Timestamp: captureTestBase, Kind: EventErrorFrame}
	if err := capture.AppendEvent(invalid); err == nil {
		t.Fatal("AppendEvent accepted an event with bus zero")
	}
	if got := capture.Len(); got != 7 {
		t.Fatalf("Len() after rejected event = %d, want 7", got)
	}
}

func TestCaptureLatestControllerState(t *testing.T) {
	// A one-record chunk seals after the first append, so pruning the records
	// that follow can discard the first state's storage while later records stay.
	capture := newTestCapture(1, 8)
	if _, ok := capture.LatestControllerState(testBus0); ok {
		t.Fatal("an empty capture reported a controller state")
	}

	at := func(ms int) time.Time { return captureTestBase.Add(time.Duration(ms) * time.Millisecond) }
	newState := func(bus BusID, ms int, state ControllerState, tx, rx uint8) Event {
		t.Helper()
		event, err := NewControllerStateEvent(bus, at(ms), state, tx, rx, true)
		if err != nil {
			t.Fatalf("NewControllerStateEvent: %v", err)
		}
		return event
	}
	busOff := newState(testBus0, 1, ControllerBusOff, 255, 3)
	warning := newState(testBus1, 2, ControllerWarning, 96, 0)
	active := newState(testBus1, 3, ControllerActive, 0, 1)
	errorFrame, err := NewErrorFrameEvent(testBus0, at(4))
	if err != nil {
		t.Fatalf("NewErrorFrameEvent: %v", err)
	}
	overrun, err := NewReceiveOverrunEvent(testBus0, at(5))
	if err != nil {
		t.Fatalf("NewReceiveOverrunEvent: %v", err)
	}
	otherBusError, err := NewErrorFrameEvent(BusID(3), at(6))
	if err != nil {
		t.Fatalf("NewErrorFrameEvent: %v", err)
	}

	// Bus 0 enters bus-off and then reports an error frame, an overrun, and a
	// frame. None of those is a state change, so bus-off must stay reported.
	for _, appendRecord := range []func() error{
		func() error { return capture.AppendEvent(busOff) },
		func() error { return capture.AppendEvent(warning) },
		func() error { return capture.AppendEvent(active) },
		func() error { return capture.AppendEvent(errorFrame) },
		func() error { return capture.AppendEvent(overrun) },
		func() error { return capture.AppendEvent(otherBusError) },
		func() error {
			return capture.Append(testDataEvent(t, testBus0, 0x100, 0, []byte{1}, 7, DirectionReceive))
		},
	} {
		if err := appendRecord(); err != nil {
			t.Fatalf("append record: %v", err)
		}
	}

	requireState := func(label string, bus BusID, want Event) {
		t.Helper()
		got, ok := capture.LatestControllerState(bus)
		if !ok {
			t.Fatalf("%s: bus %d has no controller state", label, bus)
		}
		requireBusEvents(t, label, []Event{got}, []Event{want})
	}
	requireState("bus 0 after later non-state events", testBus0, busOff)
	requireState("bus 1 newest state", testBus1, active)
	if _, ok := capture.LatestControllerState(BusID(3)); ok {
		t.Fatal("a bus with only an error frame reported a controller state")
	}

	// As with Latest, discarding the records keeps the stored copy available.
	if err := capture.Prune(capture.End()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, event := range collectAllBusEvents(t, capture, testBus0) {
		if event.Kind == EventControllerState {
			t.Fatalf("bus 0 events after Prune = %+v, want the bus-off record discarded", event)
		}
	}
	requireState("bus 0 after Prune", testBus0, busOff)
	requireState("bus 1 after Prune", testBus1, active)

	capture.Clear()
	for _, bus := range []BusID{testBus0, testBus1} {
		if _, ok := capture.LatestControllerState(bus); ok {
			t.Fatalf("Clear kept the controller state of bus %d", bus)
		}
	}
}

func TestCaptureBetween(t *testing.T) {
	capture := NewCapture()
	frames := []FrameEvent{
		testDataEvent(t, testBus0, 0x100, 0, []byte{0}, 0, DirectionReceive),
		testDataEvent(t, testBus0, 0x100, 0, []byte{1}, 1, DirectionReceive),
		testDataEvent(t, testBus0, 0x100, 0, []byte{2}, 2, DirectionReceive),
	}
	if err := capture.Append(frames[0]); err != nil {
		t.Fatalf("append before range: %v", err)
	}
	start := capture.End()
	if err := capture.Append(frames[1]); err != nil {
		t.Fatalf("append in range: %v", err)
	}
	end := capture.End()
	if err := capture.Append(frames[2]); err != nil {
		t.Fatalf("append after range: %v", err)
	}

	betweenSeq, betweenCursor, err := capture.Frames(Between(start, end))
	if err != nil {
		t.Fatalf("Frames(Between): %v", err)
	}
	if betweenCursor != end {
		t.Fatalf("Frames(Between) cursor = %+v, want %+v", betweenCursor, end)
	}
	requireEvents(t, "Frames(Between)", slices.Collect(betweenSeq), frames[1:2])

	if seq, gotCur, err := capture.Frames(Between(end, end)); err != nil || gotCur != end || len(slices.Collect(seq)) != 0 {
		t.Fatalf("Frames(Between) over an empty interval = %+v, %v", gotCur, err)
	}
	if seq, gotCur, err := capture.Series(frames[0].Key(), Between(Cursor{}, Cursor{})); err != nil || gotCur != (Cursor{}) || len(slices.Collect(seq)) != 0 {
		t.Fatalf("Series(Between) over zero interval = %+v, %v", gotCur, err)
	}
	if seq, gotCur, err := capture.BusEvents(testBus0, Between(Cursor{}, Cursor{})); err != nil || gotCur != (Cursor{}) || len(slices.Collect(seq)) != 0 {
		t.Fatalf("BusEvents(Between) over zero interval = %+v, %v", gotCur, err)
	}

	// Single-chunk indexed between: matching records before start, inside (start, end], and after end.
	singleSeriesSeq, gotCur, err := capture.Series(frames[0].Key(), Between(start, end))
	if err != nil || gotCur != end {
		t.Fatalf("Series(Between) single chunk: %+v, %v", gotCur, err)
	}
	requireEvents(t, "Series(Between) single chunk", slices.Collect(singleSeriesSeq), frames[1:2])

	// Multi-chunk indexed Series(Between) and BusEvents(Between) across sealed,
	// gap, and active chunks with matching records both before start and after end.
	multi := newTestCapture(3, 24)
	keyA := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
	keyB := FrameKey{Bus: testBus0, ID: 0x200, Direction: DirectionReceive}
	at := func(seq int) time.Time { return captureTestBase.Add(time.Duration(seq) * time.Millisecond) }
	appendErr := func(bus BusID, seq int) Event {
		t.Helper()
		ev, err := NewErrorFrameEvent(bus, at(seq))
		if err != nil {
			t.Fatalf("NewErrorFrameEvent: %v", err)
		}
		if err := multi.AppendEvent(ev); err != nil {
			t.Fatalf("AppendEvent %d: %v", seq, err)
		}
		return ev
	}
	appendFrame := func(id uint32, seq int) FrameEvent {
		t.Helper()
		ev := testDataEvent(t, testBus0, id, 0, []byte{byte(seq), 0, 0, 0, 0, 0, 0, 0}, seq, DirectionReceive)
		if err := multi.Append(ev); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
		return ev
	}

	// Chunk 0 (cap 3 records, 24 B payload): A0, ev0 (bus0), A1 -> start after A0
	appendFrame(keyA.ID, 0)
	rangeStart := multi.End()
	wantEv0 := appendErr(testBus0, 1)
	wantA1 := appendFrame(keyA.ID, 2)

	// Seal chunk 0 and an entire middle chunk (chunk 1) with NO keyA or testBus0
	// events by filling with keyB until chunk 2 becomes active.
	for seq := 3; len(multi.chunks) < 3; seq++ {
		appendFrame(keyB.ID, seq)
	}
	// Now in chunk 2: add keyB + testBus1, then keyA + testBus0, then mark end,
	// then many more keyA + testBus0 records after end.
	appendFrame(keyB.ID, 200)
	appendErr(testBus1, 201)
	wantA2 := appendFrame(keyA.ID, 202)
	wantEv1 := appendErr(testBus0, 203)
	rangeEnd := multi.End()
	// Append many more matching records AFTER rangeEnd in the same active chunk
	// so the narrow tail range triggers the dense forward-scan path.
	for seq := 204; seq < 240; seq++ {
		appendFrame(keyA.ID, seq)
		appendErr(testBus0, seq)
	}

	gotSeriesSeq, gotSeriesCur, err := multi.Series(keyA, Between(rangeStart, rangeEnd))
	if err != nil || gotSeriesCur != rangeEnd {
		t.Fatalf("Series(Between) multi-chunk: %+v, %v", gotSeriesCur, err)
	}
	requireEvents(t, "Series(Between) multi-chunk", slices.Collect(gotSeriesSeq), []FrameEvent{wantA1, wantA2})

	gotBusSeq, gotBusCur, err := multi.BusEvents(testBus0, Between(rangeStart, rangeEnd))
	if err != nil || gotBusCur != rangeEnd {
		t.Fatalf("BusEvents(Between) multi-chunk: %+v, %v", gotBusCur, err)
	}
	requireBusEvents(t, "BusEvents(Between) multi-chunk", slices.Collect(gotBusSeq), []Event{wantEv0, wantEv1})

	// Prune chunk 0 and verify (Cursor{}, pruneSeam] and (pruneSeam, pruneSeam] are empty.
	sealedEnd := Cursor{
		generation: multi.generation,
		chunk:      multi.chunks[0].sequence,
		record:     uint32(len(multi.chunks[0].records) - 1),
	}
	if err := multi.Prune(sealedEnd); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, pair := range [][2]Cursor{{Cursor{}, sealedEnd}, {sealedEnd, sealedEnd}, {sealedEnd, Cursor{}}} {
		if seq, gotCur, err := multi.Frames(Between(pair[0], pair[1])); err != nil || gotCur != pair[1] || len(slices.Collect(seq)) != 0 {
			t.Fatalf("Frames(Between(%+v, %+v)) = %+v, %v", pair[0], pair[1], gotCur, err)
		}
		if seq, gotCur, err := multi.Series(keyA, Between(pair[0], pair[1])); err != nil || gotCur != pair[1] || len(slices.Collect(seq)) != 0 {
			t.Fatalf("Series(Between(%+v, %+v)) = %+v, %v", pair[0], pair[1], gotCur, err)
		}
		if seq, gotCur, err := multi.BusEvents(testBus0, Between(pair[0], pair[1])); err != nil || gotCur != pair[1] || len(slices.Collect(seq)) != 0 {
			t.Fatalf("BusEvents(Between(%+v, %+v)) = %+v, %v", pair[0], pair[1], gotCur, err)
		}
	}
}

func TestCaptureCursorErrors(t *testing.T) {
	capture := newTestCapture(4, 24)
	key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
	var appended []FrameEvent
	appendFrame := func(seq int) {
		t.Helper()
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data, uint32(seq))
		event := testDataEvent(t, testBus0, 0x100, 0, data, seq, DirectionReceive)
		if err := capture.Append(event); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
		appended = append(appended, event)
	}
	appendFrame(0)
	early := capture.End()
	for seq := 1; seq < 6; seq++ {
		appendFrame(seq)
	}
	end := capture.End()
	if end.chunk == 0 {
		t.Fatalf("fixture stayed inside one chunk, want it to rotate")
	}

	spanningSeq, _, err := capture.Frames(Between(early, end))
	if err != nil {
		t.Fatalf("Frames(Between) across a seam: %v", err)
	}
	requireEvents(t, "Frames(Between) across a seam", slices.Collect(spanningSeq), appended[1:])
	seriesSeq, _, err := capture.Series(key, Between(early, end))
	if err != nil {
		t.Fatalf("Series(Between) across a seam: %v", err)
	}
	requireEvents(t, "Series(Between) across a seam", slices.Collect(seriesSeq), appended[1:])

	beyondSealed := Cursor{
		generation: early.generation,
		chunk:      early.chunk,
		record:     uint32(len(capture.chunks[0].records)),
	}
	if _, _, err := capture.Frames(Since(beyondSealed)); err == nil {
		t.Fatal("Frames(Since) accepted cursor beyond sealed chunk length")
	} else {
		requireRejectedCursor(t, err, beyondSealed)
	}
	if _, _, err := capture.Series(key, Between(beyondSealed, end)); err == nil {
		t.Fatal("Series(Between) accepted start cursor beyond sealed chunk length")
	} else {
		requireRejectedCursor(t, err, beyondSealed)
	}

	other := NewCapture()
	if err := other.Append(testDataEvent(t, testBus0, 0x100, 0, []byte{9}, 0, DirectionReceive)); err != nil {
		t.Fatalf("append to the other capture: %v", err)
	}
	foreign := other.End()
	if seq, gotCur, err := capture.Frames(Between(end, early)); !errors.Is(err, ErrCursorOutOfRange) || seq != nil || gotCur != end {
		t.Fatalf("Frames(Between) over reversed range = (%t, %+v, %v)", seq != nil, gotCur, err)
	} else {
		var rejected *CursorOutOfRangeError
		if errors.As(err, &rejected) {
			t.Fatalf("reversed range identifies a placeable cursor: %+v", rejected.Cursor)
		}
	}

	capture.Clear()
	appendFrame(6)
	beyondActive := capture.End()
	beyondActive.record++
	for _, cursor := range []Cursor{foreign, early, beyondActive} {
		writer := &captureWriterProbe{failAt: -1}
		since := []struct {
			name string
			read func() (bool, Cursor, error)
		}{
			{"Frames(Since)", func() (bool, Cursor, error) {
				seq, next, err := capture.Frames(Since(cursor))
				return seq != nil, next, err
			}},
			{"Events(Since)", func() (bool, Cursor, error) {
				seq, next, err := capture.Events(Since(cursor))
				return seq != nil, next, err
			}},
			{"Series(Since)", func() (bool, Cursor, error) {
				seq, next, err := capture.Series(key, Since(cursor))
				return seq != nil, next, err
			}},
			{"BusEvents(Since)", func() (bool, Cursor, error) {
				seq, next, err := capture.BusEvents(testBus0, Since(cursor))
				return seq != nil, next, err
			}},
			{"WriteRecords(Since)", func() (bool, Cursor, error) {
				next, err := capture.WriteRecords(Since(cursor), writer)
				return len(writer.frames)+len(writer.events) != 0, next, err
			}},
		}
		for _, read := range since {
			hasData, next, err := read.read()
			requireRejectedCursor(t, err, cursor)
			if hasData || next != cursor {
				t.Fatalf("%s with rejected cursor = (hasData=%t, %+v), want no data and %+v", read.name, hasData, next, cursor)
			}
		}
		if event, next, ok, err := capture.FirstSince(key, cursor); err == nil || ok || event != (FrameEvent{}) || next != cursor {
			t.Fatalf("FirstSince with rejected cursor = (%+v, %+v, %t, %v)", event, next, ok, err)
		} else {
			requireRejectedCursor(t, err, cursor)
		}
		event, next, err := capture.Next(context.Background(), key, cursor)
		requireRejectedCursor(t, err, cursor)
		if event != (FrameEvent{}) || next != cursor {
			t.Fatalf("Next with rejected cursor = %+v, %+v", event, next)
		}
		retained := capture.Len()
		requireRejectedCursor(t, capture.Prune(capture.End(), Cursor{}, cursor), cursor)
		if capture.Len() != retained {
			t.Fatal("Prune discarded records despite a rejected cursor")
		}
		between := []struct {
			name string
			read func(Cursor, Cursor) (bool, Cursor, error)
		}{
			{"Frames(Between)", func(start, end Cursor) (bool, Cursor, error) {
				seq, next, err := capture.Frames(Between(start, end))
				return seq != nil, next, err
			}},
			{"Events(Between)", func(start, end Cursor) (bool, Cursor, error) {
				seq, next, err := capture.Events(Between(start, end))
				return seq != nil, next, err
			}},
			{"Series(Between)", func(start, end Cursor) (bool, Cursor, error) {
				seq, next, err := capture.Series(key, Between(start, end))
				return seq != nil, next, err
			}},
			{"BusEvents(Between)", func(start, end Cursor) (bool, Cursor, error) {
				seq, next, err := capture.BusEvents(testBus0, Between(start, end))
				return seq != nil, next, err
			}},
			{"WriteRecords(Between)", func(start, end Cursor) (bool, Cursor, error) {
				next, err := capture.WriteRecords(Between(start, end), writer)
				return len(writer.frames)+len(writer.events) != 0, next, err
			}},
		}
		for _, read := range between {
			for _, bounds := range [][2]Cursor{{cursor, capture.End()}, {Cursor{}, cursor}} {
				hasData, next, err := read.read(bounds[0], bounds[1])
				requireRejectedCursor(t, err, cursor)
				if hasData || next != bounds[0] {
					t.Fatalf("%s with rejected cursor = (hasData=%t, %+v), want no data and %+v", read.name, hasData, next, bounds[0])
				}
			}
		}
	}
}

func requireRejectedCursor(t *testing.T, err error, cursor Cursor) {
	t.Helper()
	var rejected *CursorOutOfRangeError
	if !errors.Is(err, ErrCursorOutOfRange) || !errors.As(err, &rejected) || rejected.Cursor != cursor {
		t.Fatalf("error = %v, want CursorOutOfRangeError for %+v wrapping ErrCursorOutOfRange", err, cursor)
	}
}

func TestCaptureWriteRecordsFailureCursor(t *testing.T) {
	capture := newTestCapture(2, 8)
	frame0 := testDataEvent(t, testBus0, 0x100, 0, []byte{1}, 0, DirectionReceive)
	state, err := NewControllerStateEvent(
		testBus0,
		captureTestBase.Add(time.Millisecond),
		ControllerWarning,
		1,
		2,
		true,
	)
	if err != nil {
		t.Fatalf("NewControllerStateEvent: %v", err)
	}
	frame1 := testDataEvent(t, testBus0, 0x200, 0, []byte{2}, 2, DirectionReceive)
	errorEvent, err := NewErrorFrameEvent(testBus0, captureTestBase.Add(3*time.Millisecond))
	if err != nil {
		t.Fatalf("NewErrorFrameEvent: %v", err)
	}
	for _, appendRecord := range []func() error{
		func() error { return capture.Append(frame0) },
		func() error { return capture.AppendEvent(state) },
		func() error { return capture.Append(frame1) },
		func() error { return capture.AppendEvent(errorEvent) },
	} {
		if err := appendRecord(); err != nil {
			t.Fatalf("append record: %v", err)
		}
	}

	writeFailure := errors.New("writer failed")
	first := &captureWriterProbe{failAt: 2, failure: writeFailure}
	written, err := capture.WriteRecords(Range{}, first)
	var recordError *RecordWriteError
	if !errors.As(err, &recordError) {
		t.Fatalf("WriteRecords error = %v, want *RecordWriteError", err)
	}
	if !errors.Is(err, writeFailure) {
		t.Fatalf("WriteRecords error = %v, want wrapped writer failure", err)
	}
	if len(first.frames) != 1 || first.frames[0] != frame0.Frame.ID ||
		len(first.events) != 1 || first.events[0] != state.Kind {
		t.Fatalf("records before failure = frames %v events %v", first.frames, first.events)
	}
	if written == capture.End() || written == recordError.Cursor {
		t.Fatal("failure did not distinguish the last written and failed record cursors")
	}

	retry := &captureWriterProbe{failAt: -1}
	next, err := capture.WriteRecords(Since(written), retry)
	if err != nil {
		t.Fatalf("retry WriteRecords: %v", err)
	}
	if len(retry.frames) != 1 || retry.frames[0] != frame1.Frame.ID ||
		len(retry.events) != 1 || retry.events[0] != errorEvent.Kind {
		t.Fatalf("retried records = frames %v events %v", retry.frames, retry.events)
	}
	if next != capture.End() {
		t.Fatal("successful retry did not advance to the capture end")
	}

	skip := &captureWriterProbe{failAt: -1}
	if _, err := capture.WriteRecords(Since(recordError.Cursor), skip); err != nil {
		t.Fatalf("skip failed record: %v", err)
	}
	if len(skip.frames) != 0 || len(skip.events) != 1 || skip.events[0] != errorEvent.Kind {
		t.Fatalf("records after skipped failure = frames %v events %v", skip.frames, skip.events)
	}
}

func TestCaptureCursorIncremental(t *testing.T) {
	capture := newTestCapture(4, 24)

	keyA := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
	appendSeq := func(seq int, id uint32) FrameEvent {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data, uint32(seq))
		event := testDataEvent(t, testBus0, id, 0, data, seq, DirectionReceive)
		if err := capture.Append(event); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
		return event
	}
	appendBatch := func(from, to int) (all, matchingA []FrameEvent) {
		for seq := from; seq < to; seq++ {
			id := uint32(0x100)
			if seq%2 == 1 {
				id = 0x200
			}
			event := appendSeq(seq, id)
			all = append(all, event)
			if id == 0x100 {
				matchingA = append(matchingA, event)
			}
		}
		return all, matchingA
	}

	initialAll, initialA := appendBatch(0, 4)

	readSeries := func(what string, cursor Cursor) ([]FrameEvent, Cursor) {
		t.Helper()
		seq, next, err := capture.Series(keyA, Since(cursor))
		if err != nil {
			t.Fatalf("%s Series: %v", what, err)
		}
		return slices.Collect(seq), next
	}
	readFrames := func(what string, cursor Cursor) ([]FrameEvent, Cursor) {
		t.Helper()
		seq, next, err := capture.Frames(Since(cursor))
		if err != nil {
			t.Fatalf("%s Frames: %v", what, err)
		}
		return slices.Collect(seq), next
	}

	seriesA, cursorA := readSeries("initial", Cursor{})
	requireEvents(t, "initial Series(Since)", seriesA, initialA)
	frames, cursorAll := readFrames("initial", Cursor{})
	requireEvents(t, "initial Frames(Since)", frames, initialAll)

	if cursorA != cursorAll {
		t.Fatalf("Series(Since) cursor %+v differs from Frames(Since) cursor %+v at the same tail", cursorA, cursorAll)
	}

	if frames, cursor := readSeries("tail", cursorA); len(frames) != 0 || cursor != cursorA {
		t.Fatalf("Series(Since) at tail returned %d frames (cursor changed: %t)", len(frames), cursor != cursorA)
	}
	if frames, cursor := readFrames("tail", cursorAll); len(frames) != 0 || cursor != cursorAll {
		t.Fatalf("Frames(Since) at tail returned %d frames (cursor changed: %t)", len(frames), cursor != cursorAll)
	}

	freshAll, freshA := appendBatch(4, 12)
	if got := len(capture.chunks); got < 2 {
		t.Fatalf("capture holds %d chunks, want the increments to span a rotation", got)
	}

	seriesA, cursorA = readSeries("incremental", cursorA)
	requireEvents(t, "incremental Series(Since) across seam", seriesA, freshA)
	frames, _ = readFrames("incremental", cursorAll)
	requireEvents(t, "incremental Frames(Since) across seam", frames, freshAll)

	appendSeq(50, 0x200)
	quiet, cursorA := readSeries("quiet", cursorA)
	if len(quiet) != 0 {
		t.Fatalf("Series(Since) returned %d frames for a series with no new traffic", len(quiet))
	}
	if replayed, _ := readFrames("quiet", cursorA); len(replayed) != 0 {
		t.Fatalf("cursor regressed: Frames(Since) replayed %d frames", len(replayed))
	}

	capture.Clear()
	revived := appendSeq(100, 0x100)
	stale, returned, err := capture.Series(keyA, Since(cursorA))
	if !errors.Is(err, ErrCursorOutOfRange) {
		t.Fatalf("Series after Clear error = %v, want ErrCursorOutOfRange", err)
	}
	if stale != nil || returned != cursorA {
		t.Fatalf("stale Series returned (%t, %+v)", stale != nil, returned)
	}
	seriesA, _ = readSeries("resynchronised", Cursor{})
	requireEvents(t, "resynchronised after Clear", seriesA, []FrameEvent{revived})
}

func TestCapturePrune(t *testing.T) {
	capture := newTestCapture(4, 24)
	key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
	var appended []FrameEvent
	var cursors []Cursor
	quiet := FrameKey{Bus: testBus0, ID: 0x200, Direction: DirectionReceive}
	for seq := range 12 {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data, uint32(seq))
		id := key.ID
		if seq == 0 {
			id = quiet.ID
		}
		event := testDataEvent(t, testBus0, id, 0, data, seq, DirectionReceive)
		if err := capture.Append(event); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
		appended = append(appended, event)
		cursors = append(cursors, capture.End())
	}

	// Pruning must leave the sealed chunk collectable, which no read reveals.
	// The chunk pointer stays out of this frame, where a live stack slot could
	// pin it by itself.
	released := make(chan struct{})
	var sealed int
	func() {
		chunk := capture.chunks[0]
		sealed = len(chunk.records)
		runtime.AddCleanup(chunk, func(done chan struct{}) { close(done) }, released)
	}()
	if len(capture.chunks) != 2 || sealed == len(appended) {
		t.Fatalf("capture holds %d chunks with %d sealed records, want a sealed chunk and an active one",
			len(capture.chunks), sealed)
	}

	if err := capture.Prune(cursors[sealed-2]); err != nil || capture.Len() != len(appended) {
		t.Fatalf("Prune inside the first chunk = %v, retaining %d records", err, capture.Len())
	}

	if err := capture.Prune(cursors[sealed-1]); err != nil {
		t.Fatalf("Prune at the chunk seam: %v", err)
	}
	if capture.Len() != len(appended)-sealed {
		t.Fatalf("capture retains %d records, want %d", capture.Len(), len(appended)-sealed)
	}

	for _, from := range []struct {
		label  string
		cursor Cursor
		want   []FrameEvent
	}{
		{"the zero Cursor", Cursor{}, appended[sealed:]},
		{"the prune-point cursor", cursors[sealed-1], appended[sealed:]},
		{"a retained cursor", cursors[sealed+1], appended[sealed+2:]},
	} {
		seq, _, err := capture.Frames(Since(from.cursor))
		if err != nil {
			t.Fatalf("Frames(Since) %s after pruning: %v", from.label, err)
		}
		requireEvents(t, "Frames(Since) "+from.label+" after pruning", slices.Collect(seq), from.want)
	}

	event, _, err := capture.Next(context.Background(), key, cursors[sealed-1])
	if err != nil || !eventsEqual(event, appended[sealed]) {
		t.Fatalf("Next from the prune-point cursor = (%+v, %v), want %+v", event, err, appended[sealed])
	}
	betweenSeq, _, err := capture.Frames(Between(cursors[sealed-1], capture.End()))
	if err != nil {
		t.Fatalf("Frames(Between) from the prune-point cursor: %v", err)
	}
	requireEvents(t, "Frames(Between) from the prune-point cursor", slices.Collect(betweenSeq), appended[sealed:])

	stale, returned, err := capture.Frames(Since(cursors[0]))
	if !errors.Is(err, ErrCursorOutOfRange) || stale != nil || returned != cursors[0] {
		t.Fatalf("Frames(Since) a discarded cursor = (%t, cursor %+v, %v)", stale != nil, returned, err)
	}
	if err := capture.Prune(cursors[0]); !errors.Is(err, ErrCursorOutOfRange) {
		t.Fatalf("Prune with a discarded cursor = %v, want ErrCursorOutOfRange", err)
	}

	if latest, ok := capture.Latest(key); !ok || !eventsEqual(latest, appended[len(appended)-1]) {
		t.Fatalf("Latest after pruning = %+v (ok=%t), want the newest appended frame", latest, ok)
	}
	if latest, ok := capture.Latest(quiet); !ok || !eventsEqual(latest, appended[0]) {
		t.Fatalf("Latest of a discarded ID = %+v (ok=%t), want its last frame", latest, ok)
	}

	if err := capture.Prune(capture.End()); err != nil || capture.Len() != len(appended)-sealed {
		t.Fatalf("Prune at the capture end = %v, retaining %d records", err, capture.Len())
	}

	collected := false
	for deadline := time.Now().Add(10 * time.Second); !collected && time.Now().Before(deadline); {
		runtime.GC()
		select {
		case <-released:
			collected = true
		default:
			runtime.Gosched()
		}
	}
	if !collected {
		t.Fatalf("a discarded chunk is still referenced, with %d records retained", capture.Len())
	}
	// The capture has to outlive the collections above, or the discarded chunk
	// would go with it and prove nothing.
	runtime.KeepAlive(capture)
}

func TestCapturePruneFollowsSlowestCursor(t *testing.T) {
	capture := newTestCapture(4, 24)
	var cursors []Cursor
	for seq := range 8 {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data, uint32(seq))
		if err := capture.Append(testDataEvent(t, testBus0, 0x100, 0, data, seq, DirectionReceive)); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
		cursors = append(cursors, capture.End())
	}
	if cursors[len(cursors)-1].chunk == 0 {
		t.Fatal("fixture stayed inside one chunk, want it to rotate")
	}

	retained := capture.Len()
	if err := capture.Prune(); err != nil || capture.Len() != retained {
		t.Fatalf("empty Prune = %v, retaining %d records, want %d", err, capture.Len(), retained)
	}
	if err := capture.Prune(cursors[5], Cursor{}); err != nil || capture.Len() != retained {
		t.Fatalf("Prune with a zero cursor = %v, retaining %d records, want %d", err, capture.Len(), retained)
	}

	if err := capture.Prune(cursors[5], cursors[1]); err != nil || capture.Len() != retained {
		t.Fatalf("Prune behind the slowest consumer = %v, retaining %d records, want %d", err, capture.Len(), retained)
	}

	sealed := len(capture.chunks[0].records)
	if err := capture.Prune(cursors[5], cursors[sealed-1]); err != nil {
		t.Fatalf("Prune after the slowest consumer advanced: %v", err)
	}
	retained -= sealed
	if capture.Len() != retained {
		t.Fatalf("Prune after both consumers advanced retained %d records, want %d", capture.Len(), retained)
	}

	if err := capture.Prune(cursors[5], cursors[sealed-1]); err != nil || capture.Len() != retained {
		t.Fatalf("repeated Prune = %v, retaining %d records, want %d", err, capture.Len(), retained)
	}

	if err := capture.Prune(cursors[5], cursors[0]); !errors.Is(err, ErrCursorOutOfRange) || capture.Len() != retained {
		t.Fatalf("Prune with a stale consumer = %v, retaining %d records, want ErrCursorOutOfRange and %d",
			err, capture.Len(), retained)
	} else {
		requireRejectedCursor(t, err, cursors[0])
	}

	if err := capture.Prune(cursors[5], Cursor{}); err != nil || capture.Len() != retained {
		t.Fatalf("Prune with a recovered consumer = %v, retaining %d records, want %d", err, capture.Len(), retained)
	}
}

func TestCaptureNext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capture := NewCapture()
		key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}

		first := testDataEvent(t, testBus0, 0x100, 0, []byte{1}, 0, DirectionReceive)
		second := testDataEvent(t, testBus0, 0x100, 0, []byte{2}, 1, DirectionReceive)
		for _, event := range []FrameEvent{first, second} {
			if err := capture.Append(event); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}

		event, cursor, err := capture.Next(context.Background(), key, Cursor{})
		if err != nil || !eventsEqual(event, first) {
			t.Fatalf("first Next = (%+v, %v), want first event", event, err)
		}
		event, cursor, err = capture.Next(context.Background(), key, cursor)
		if err != nil || !eventsEqual(event, second) {
			t.Fatalf("second Next = (%+v, %v), want second event", event, err)
		}

		end := capture.End()
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := capture.Next(cancelled, key, end); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Next error = %v, want context.Canceled", err)
		}

		type nextResult struct {
			event  FrameEvent
			cursor Cursor
			err    error
		}
		results := make(chan nextResult, 2)
		waitContext, stopWaiting := context.WithTimeout(context.Background(), time.Second)
		defer stopWaiting()
		for range 2 {
			go func() {
				event, cursor, err := capture.Next(waitContext, key, end)
				results <- nextResult{event: event, cursor: cursor, err: err}
			}()
		}
		synctest.Wait()

		third := testDataEvent(t, testBus0, 0x100, 0, []byte{3}, 2, DirectionReceive)
		if err := capture.Append(third); err != nil {
			t.Fatalf("Append third: %v", err)
		}
		synctest.Wait()
		for range 2 {
			var result nextResult
			select {
			case result = <-results:
			default:
				t.Fatal("Next waiter did not wake")
			}
			if result.err != nil || !eventsEqual(result.event, third) {
				t.Fatalf("waiting Next = (%+v, %v), want third event", result.event, result.err)
			}
			if result.cursor != capture.End() {
				t.Fatalf("waiting Next cursor = %+v, want current end %+v", result.cursor, capture.End())
			}
		}
	})
}

func TestCaptureNextAcrossClear(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capture := NewCapture()
		key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
		other := FrameKey{Bus: testBus0, ID: 0x200, Direction: DirectionReceive}
		if err := capture.Append(testDataEvent(t, testBus0, key.ID, 0, []byte{1}, 0, DirectionReceive)); err != nil {
			t.Fatalf("append first frame: %v", err)
		}
		held := capture.End()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		type result struct {
			event  FrameEvent
			cursor Cursor
			err    error
		}
		pending := make(chan result, 1)
		go func() {
			event, next, err := capture.Next(ctx, key, held)
			pending <- result{event, next, err}
		}()
		synctest.Wait()
		capture.Clear()
		synctest.Wait()

		select {
		case got := <-pending:
			t.Fatalf("registered Next returned (%+v, %v) during Clear", got.event, got.err)
		default:
		}

		if err := capture.Append(testDataEvent(t, testBus0, other.ID, 0, []byte{2}, 1, DirectionReceive)); err != nil {
			t.Fatalf("append unrelated frame: %v", err)
		}
		synctest.Wait()
		select {
		case got := <-pending:
			t.Fatalf("registered Next returned unrelated frame (%+v, %v)", got.event, got.err)
		default:
		}

		revived := testDataEvent(t, testBus0, key.ID, 0, []byte{3}, 2, DirectionReceive)
		if err := capture.Append(revived); err != nil {
			t.Fatalf("append after Clear: %v", err)
		}
		got := <-pending
		if got.err != nil || !eventsEqual(got.event, revived) {
			t.Fatalf("registered Next across Clear = (%+v, %v), want %+v", got.event, got.err, revived)
		}
		if got.cursor != capture.End() {
			t.Fatalf("registered Next cursor = %+v, want %+v", got.cursor, capture.End())
		}

		if event, next, err := capture.Next(context.Background(), key, held); !errors.Is(err, ErrCursorOutOfRange) || event != (FrameEvent{}) || next != held {
			t.Fatalf("new Next from cleared cursor = (%+v, %+v, %v), want ErrCursorOutOfRange", event, next, err)
		}
	})
}

func TestCaptureNextAcrossPrune(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capture := newTestCapture(4, 24)
		key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
		other := FrameKey{Bus: testBus0, ID: 0x200, Direction: DirectionReceive}
		var discarded, sealed Cursor
		for seq := range 3 {
			id := other.ID
			if seq == 0 {
				id = key.ID
			}
			data := make([]byte, 8)
			binary.LittleEndian.PutUint32(data, uint32(seq))
			event := testDataEvent(t, testBus0, id, 0, data, seq, DirectionReceive)
			if err := capture.Append(event); err != nil {
				t.Fatalf("Append %d: %v", seq, err)
			}
			if seq == 0 {
				discarded = capture.End()
			}
			sealed = capture.End()
		}
		if discarded == sealed {
			t.Fatal("sealed chunk has one record, want a discarded cursor distinct from the prune point")
		}
		if err := capture.Append(testDataEvent(t, testBus0, other.ID, 0, make([]byte, 8), 3, DirectionReceive)); err != nil {
			t.Fatalf("append unrelated frame: %v", err)
		}
		if len(capture.chunks) != 2 {
			t.Fatalf("capture holds %d chunks, want a sealed chunk and an active one", len(capture.chunks))
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		type result struct {
			event  FrameEvent
			cursor Cursor
			err    error
		}
		stale := make(chan result, 1)
		seam := make(chan result, 1)
		go func() {
			event, next, err := capture.Next(ctx, key, discarded)
			stale <- result{event, next, err}
		}()
		go func() {
			event, next, err := capture.Next(ctx, key, sealed)
			seam <- result{event, next, err}
		}()
		synctest.Wait()

		if err := capture.Prune(sealed); err != nil {
			t.Fatalf("Prune at the chunk seam: %v", err)
		}
		synctest.Wait()

		for label, result := range map[string]<-chan result{
			"discarded-cursor Next": stale,
			"prune-point Next":      seam,
		} {
			select {
			case got := <-result:
				t.Fatalf("%s returned (%+v, %v) during Prune", label, got.event, got.err)
			default:
			}
		}

		revived := testDataEvent(t, testBus0, key.ID, 0, []byte{4}, 4, DirectionReceive)
		if err := capture.Append(revived); err != nil {
			t.Fatalf("append after Prune: %v", err)
		}
		for label, result := range map[string]<-chan result{
			"discarded-cursor Next": stale,
			"prune-point Next":      seam,
		} {
			got := <-result
			if got.err != nil || !eventsEqual(got.event, revived) || got.cursor != capture.End() {
				t.Fatalf("%s = (%+v, %+v, %v), want %+v at %+v", label, got.event, got.cursor, got.err, revived, capture.End())
			}
		}

		if event, next, err := capture.Next(context.Background(), key, discarded); !errors.Is(err, ErrCursorOutOfRange) || event != (FrameEvent{}) || next != discarded {
			t.Fatalf("new Next from pruned cursor = (%+v, %+v, %v), want ErrCursorOutOfRange", event, next, err)
		}
	})
}

func TestCaptureConcurrentClear(t *testing.T) {
	capture := newTestCapture(8, 64)

	const writers = 2
	const perWriter = 400

	var writing sync.WaitGroup
	for w := range writers {
		writing.Add(1)
		go func() {
			defer writing.Done()
			bus := BusID(w + 1)
			for seq := range perWriter {
				data := make([]byte, 8)
				binary.LittleEndian.PutUint32(data, uint32(seq))
				frame, err := NewFrame(0x100, data, 0)
				if err != nil {
					t.Errorf("NewFrame: %v", err)
					return
				}
				event := FrameEvent{
					Bus:       bus,
					Timestamp: captureTestBase.Add(time.Duration(seq) * time.Microsecond),
					Direction: DirectionReceive,
					Frame:     frame,
				}
				if err := capture.Append(event); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	writing.Add(1)
	go func() {
		defer writing.Done()
		for seq := range perWriter {
			event, err := NewControllerStateEvent(
				testBus0,
				captureTestBase.Add(time.Duration(seq)*time.Microsecond),
				ControllerActive,
				uint8(seq),
				uint8(seq),
				true,
			)
			if err != nil {
				t.Errorf("NewControllerStateEvent: %v", err)
				return
			}
			if err := capture.AppendEvent(event); err != nil {
				t.Errorf("AppendEvent: %v", err)
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		writing.Wait()
		close(done)
	}()

	var churning sync.WaitGroup
	churning.Add(3)
	go func() {
		defer churning.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			capture.Clear()
			runtime.Gosched()
		}
	}()
	go func() {
		defer churning.Done()
		var eventsCursor, busEventsCursor Cursor
		for {
			select {
			case <-done:
				return
			default:
			}
			if seq, _, err := capture.Frames(Range{}); err == nil {
				for range seq {
				}
			}
			if seq, _, err := capture.Series(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}, Range{}); err == nil {
				for range seq {
				}
			}
			capture.Latest(FrameKey{Bus: testBus1, ID: 0x100, Direction: DirectionReceive})

			seq, next, err := capture.Events(Since(eventsCursor))
			if err != nil && !errors.Is(err, ErrCursorOutOfRange) {
				t.Errorf("Events(Since): %v", err)
				return
			}
			if err == nil {
				for range seq {
				}
			}
			eventsCursor = next
			if err != nil {
				eventsCursor = Cursor{}
			}
			seq, next, err = capture.BusEvents(testBus0, Since(busEventsCursor))
			if err != nil && !errors.Is(err, ErrCursorOutOfRange) {
				t.Errorf("BusEvents(Since): %v", err)
				return
			}
			if err == nil {
				for range seq {
				}
			}
			busEventsCursor = next
			if err != nil {
				busEventsCursor = Cursor{}
			}
		}
	}()

	go func() {
		defer churning.Done()
		key := FrameKey{Bus: testBus1, ID: 0x100, Direction: DirectionReceive}
		var cursor Cursor
		for {
			select {
			case <-done:
				return
			default:
			}
			waitContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			_, next, err := capture.Next(waitContext, key, cursor)
			cancel()
			switch {
			case err == nil:
				cursor = next
			case errors.Is(err, ErrCursorOutOfRange):
				cursor = Cursor{}
			case errors.Is(err, context.DeadlineExceeded):
			default:
				t.Errorf("tailing Next under Clear: %v", err)
				return
			}
		}
	}()
	<-done
	churning.Wait()

	total := 0
	for w := range writers {
		key := FrameKey{Bus: BusID(w + 1), ID: 0x100, Direction: DirectionReceive}
		series := collectAllSeries(t, capture, key)
		total += len(series)
		for i, event := range series {
			got := binary.LittleEndian.Uint32(event.Frame.Data[:4])
			want := uint32(perWriter - len(series) + i)
			if got != want {
				t.Fatalf("series %d index %d has sequence %d, want %d", key.Bus, i, got, want)
			}
		}
		latest, ok := capture.Latest(key)
		if len(series) == 0 {
			if ok {
				t.Fatalf("Latest(%d) found a frame but the series is empty", key.Bus)
			}
			continue
		}
		if !ok || !eventsEqual(latest, series[len(series)-1]) {
			t.Fatalf("Latest(%d) does not match the series tail", key.Bus)
		}
	}
	events := collectAllBusEvents(t, capture, testBus0)
	total += len(events)
	for i, event := range events {
		got := int(event.Timestamp.Sub(captureTestBase) / time.Microsecond)
		want := perWriter - len(events) + i
		if got != want {
			t.Fatalf("event index %d has sequence %d, want %d", i, got, want)
		}
	}
	if got := capture.Len(); got != total {
		t.Fatalf("Len() = %d, want the %d records present", got, total)
	}
}

func TestCaptureConcurrent(t *testing.T) {
	capture := newTestCapture(8, 64)

	const writers = 4
	const perWriter = 500

	var writing sync.WaitGroup
	for w := range writers {
		writing.Add(1)
		go func() {
			defer writing.Done()
			bus := BusID(w + 1)
			for seq := range perWriter {
				data := make([]byte, 8)
				binary.LittleEndian.PutUint32(data, uint32(seq))
				frame, err := NewFrame(0x100, data, 0)
				if err != nil {
					t.Errorf("NewFrame: %v", err)
					return
				}
				event := FrameEvent{
					Bus:       bus,
					Timestamp: captureTestBase.Add(time.Duration(seq) * time.Microsecond),
					Direction: DirectionReceive,
					Frame:     frame,
				}
				if err := capture.Append(event); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	writing.Add(1)
	go func() {
		defer writing.Done()
		for seq := range perWriter {
			event, err := NewControllerStateEvent(
				testBus0,
				captureTestBase.Add(time.Duration(seq)*time.Microsecond),
				ControllerActive,
				uint8(seq),
				uint8(seq),
				true,
			)
			if err != nil {
				t.Errorf("NewControllerStateEvent: %v", err)
				return
			}
			if err := capture.AppendEvent(event); err != nil {
				t.Errorf("AppendEvent: %v", err)
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		writing.Wait()
		close(done)
	}()

	var reading sync.WaitGroup
	for range 2 {
		reading.Add(1)
		go func() {
			defer reading.Done()
			key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
			for {
				select {
				case <-done:
					return
				default:
				}
				if seq, _, err := capture.Frames(Range{}); err == nil {
					for range seq {
					}
				}
				if seq, _, err := capture.Series(key, Range{}); err == nil {
					for range seq {
					}
				}
				capture.Latest(FrameKey{Bus: testBus1, ID: 0x100, Direction: DirectionReceive})
				if seq, _, err := capture.Events(Range{}); err == nil {
					for range seq {
					}
				}
				if seq, _, err := capture.BusEvents(testBus0, Range{}); err == nil {
					for range seq {
					}
				}
				capture.Len()
				end := capture.End()
				framesSeq, _, framesErr := capture.Frames(Between(Cursor{}, end))
				eventsSeq, _, eventsErr := capture.Events(Between(Cursor{}, end))
				seriesSeq, _, seriesErr := capture.Series(key, Between(Cursor{}, end))
				busEventsSeq, _, busEventsErr := capture.BusEvents(testBus0, Between(Cursor{}, end))
				if err := errors.Join(framesErr, eventsErr, seriesErr, busEventsErr); err != nil {
					t.Errorf("bounded read of a live capture: %v", err)
					return
				}
				for range framesSeq {
				}
				for range eventsSeq {
				}
				for range seriesSeq {
				}
				for range busEventsSeq {
				}
			}
		}()
	}

	var tailed []FrameEvent
	tailerDone := make(chan struct{})
	go func() {
		defer close(tailerDone)
		key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
		read := func(cursor Cursor) ([]FrameEvent, Cursor, bool) {
			seq, next, err := capture.Series(key, Since(cursor))
			if err != nil {
				t.Errorf("tailing Series(Since): %v", err)
				return nil, cursor, false
			}
			return slices.Collect(seq), next, true
		}
		var cursor Cursor
		for {
			frames, next, ok := read(cursor)
			if !ok {
				return
			}
			tailed = append(tailed, frames...)
			cursor = next
			select {
			case <-done:
				frames, _, ok = read(cursor)
				if !ok {
					return
				}
				tailed = append(tailed, frames...)
				return
			default:
			}
		}
	}()

	var nextTailed []FrameEvent
	nextTailerDone := make(chan struct{})
	go func() {
		defer close(nextTailerDone)
		key := FrameKey{Bus: testBus1, ID: 0x100, Direction: DirectionReceive}
		waitContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var cursor Cursor
		for range perWriter {
			event, next, err := capture.Next(waitContext, key, cursor)
			if err != nil {
				t.Errorf("tailing Next: %v", err)
				return
			}
			nextTailed = append(nextTailed, event)
			cursor = next
		}
	}()

	var eventsTailed []Event
	eventsTailerDone := make(chan struct{})
	go func() {
		defer close(eventsTailerDone)
		read := func(cursor Cursor) ([]Event, Cursor, bool) {
			seq, next, err := capture.Events(Since(cursor))
			if err != nil {
				t.Errorf("tailing Events(Since): %v", err)
				return nil, cursor, false
			}
			return slices.Collect(seq), next, true
		}
		var cursor Cursor
		for {
			events, next, ok := read(cursor)
			if !ok {
				return
			}
			eventsTailed = append(eventsTailed, events...)
			cursor = next
			select {
			case <-done:
				events, _, ok = read(cursor)
				if !ok {
					return
				}
				eventsTailed = append(eventsTailed, events...)
				return
			default:
			}
		}
	}()

	var busEventsTailed []Event
	busEventsTailerDone := make(chan struct{})
	go func() {
		defer close(busEventsTailerDone)
		read := func(cursor Cursor) ([]Event, Cursor, bool) {
			seq, next, err := capture.BusEvents(testBus0, Since(cursor))
			if err != nil {
				t.Errorf("tailing BusEvents(Since): %v", err)
				return nil, cursor, false
			}
			return slices.Collect(seq), next, true
		}
		var cursor Cursor
		for {
			events, next, ok := read(cursor)
			if !ok {
				return
			}
			busEventsTailed = append(busEventsTailed, events...)
			cursor = next
			select {
			case <-done:
				events, _, ok = read(cursor)
				if !ok {
					return
				}
				busEventsTailed = append(busEventsTailed, events...)
				return
			default:
			}
		}
	}()

	<-done
	reading.Wait()
	<-tailerDone
	<-nextTailerDone
	<-eventsTailerDone
	<-busEventsTailerDone
	requireEvents(t, "tailed series", tailed,
		collectAllSeries(t, capture, FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}))
	requireEvents(t, "Next-tailed series", nextTailed,
		collectAllSeries(t, capture, FrameKey{Bus: testBus1, ID: 0x100, Direction: DirectionReceive}))
	requireBusEvents(t, "tailed events", eventsTailed, collectAllEvents(t, capture))
	requireBusEvents(t, "tailed bus events", busEventsTailed, collectAllBusEvents(t, capture, testBus0))

	if got := capture.Len(); got != (writers+1)*perWriter {
		t.Fatalf("Len() = %d, want %d", got, (writers+1)*perWriter)
	}
	for w := range writers {
		key := FrameKey{Bus: BusID(w + 1), ID: 0x100, Direction: DirectionReceive}
		series := collectAllSeries(t, capture, key)
		if len(series) != perWriter {
			t.Fatalf("Series(%+v) returned %d events, want %d", key, len(series), perWriter)
		}
		for seq, event := range series {
			if got := binary.LittleEndian.Uint32(event.Frame.Data[:4]); got != uint32(seq) {
				t.Fatalf("series %d out of order at %d: got sequence %d", key.Bus, seq, got)
			}
		}
	}
}

func TestCaptureFirstSinceAndNormalization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capture := NewCapture()
		key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}

		if ev, cur, ok, err := capture.FirstSince(key, Cursor{}); err != nil || ok || ev != (FrameEvent{}) || cur != (Cursor{}) {
			t.Fatalf("empty FirstSince = (%+v, %+v, %t, %v)", ev, cur, ok, err)
		}

		// Dirty trailing bytes beyond DLC and a non-UTC timestamp with monotonic reading.
		localZone := time.FixedZone("EST", -5*3600)
		dirty := FrameEvent{
			Bus:       testBus0,
			Timestamp: time.Now().In(localZone),
			Direction: DirectionReceive,
			Frame: Frame{
				ID:   0x100,
				DLC:  2,
				Data: [MaxDataLength]byte{0xAA, 0xBB, 0xFF, 0xEE, 0xDD},
			},
		}
		if err := capture.Append(dirty); err != nil {
			t.Fatalf("Append dirty: %v", err)
		}
		latest, ok := capture.Latest(key)
		if !ok {
			t.Fatal("Latest missing after Append")
		}
		for i := 2; i < MaxDataLength; i++ {
			if latest.Frame.Data[i] != 0 {
				t.Fatalf("Latest trailing byte %d = %#x, want 0", i, latest.Frame.Data[i])
			}
		}
		if latest.Timestamp.Location() != time.UTC || latest.Timestamp.String() != time.Unix(0, dirty.Timestamp.UnixNano()).UTC().String() {
			t.Fatalf("Latest timestamp = %q (%v), want UTC without monotonic clock", latest.Timestamp, latest.Timestamp.Location())
		}

		// Retained frame is returned even when Next's context is already canceled.
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		got, firstCur, err := capture.Next(cancelled, key, Cursor{})
		if err != nil || got != latest {
			t.Fatalf("Next retained with canceled ctx = (%+v, %v), want %+v", got, err, latest)
		}
		if gotFirst, firstCur2, ok, err := capture.FirstSince(key, Cursor{}); err != nil || !ok || gotFirst != latest || firstCur2 != firstCur {
			t.Fatalf("FirstSince = (%+v, %+v, %t, %v), want %+v at %+v", gotFirst, firstCur2, ok, err, latest, firstCur)
		}
		if ev, cur, ok, err := capture.FirstSince(key, firstCur); err != nil || ok || ev != (FrameEvent{}) || cur != firstCur {
			t.Fatalf("FirstSince at tail = (%+v, %+v, %t, %v)", ev, cur, ok, err)
		}

		// Verify waiting Next receives the same normalized FrameEvent (zeroed trailing bytes + UTC).
		waitCtx, stopWait := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopWait()
		type wakeResult struct {
			event FrameEvent
			err   error
		}
		woken := make(chan wakeResult, 1)
		go func() {
			ev, _, err := capture.Next(waitCtx, key, firstCur)
			woken <- wakeResult{event: ev, err: err}
		}()
		synctest.Wait()

		dirty2 := dirty
		dirty2.Frame.Data[0] = 0xCC
		dirty2.Frame.Data[7] = 0x99
		if err := capture.Append(dirty2); err != nil {
			t.Fatalf("Append dirty2: %v", err)
		}
		gotWake := <-woken
		if gotWake.err != nil {
			t.Fatalf("waiting Next: %v", gotWake.err)
		}
		if gotWake.event.Frame.Data[7] != 0 || gotWake.event.Timestamp.Location() != time.UTC {
			t.Fatalf("waiter received unnormalized frame: data[7]=%#x loc=%v", gotWake.event.Frame.Data[7], gotWake.event.Timestamp.Location())
		}
	})
}

func TestCaptureChunkGrowthLifecycle(t *testing.T) {
	capture := NewCapture()
	key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
	// Exceed the initial 256 KiB chunk (4,096 records) across multiple rotations.
	const count = 13_000
	for seq := range count {
		var data [8]byte
		binary.LittleEndian.PutUint32(data[:4], uint32(seq))
		if err := capture.Append(testDataEvent(t, testBus0, key.ID, 0, data[:], seq, DirectionReceive)); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
	}
	if len(capture.chunks) < 2 {
		t.Fatalf("chunks = %d, want geometric growth across at least 2 chunks", len(capture.chunks))
	}
	if got := capture.Len(); got != count {
		t.Fatalf("Len() = %d, want %d", got, count)
	}
	series := collectAllSeries(t, capture, key)
	if len(series) != count {
		t.Fatalf("Series len = %d, want %d", len(series), count)
	}
	for seq, ev := range series {
		if got := binary.LittleEndian.Uint32(ev.Frame.Data[:4]); got != uint32(seq) {
			t.Fatalf("series[%d] = %d, want %d", seq, got, seq)
		}
	}
	if err := capture.Prune(capture.End()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(capture.chunks) != 1 {
		t.Fatalf("chunks after Prune(End) = %d, want 1", len(capture.chunks))
	}
}

func TestCaptureStreamingContracts(t *testing.T) {
	// Bounded empty interval from an initially empty capture stays empty even
	// after new records are appended, unlike the zero Range.
	empty := NewCapture()
	emptyEnd := empty.End()
	if err := empty.Append(testDataEvent(t, testBus0, 0x100, 0, []byte{1}, 0, DirectionReceive)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if seq, cur, err := empty.Frames(Between(Cursor{}, emptyEnd)); err != nil || cur != emptyEnd || len(slices.Collect(seq)) != 0 {
		t.Fatalf("Between(Cursor{}, emptyEnd) after append = (%+v, %v), want empty", cur, err)
	}
	if seq, cur, err := empty.Frames(Range{}); err != nil || cur != empty.End() || len(slices.Collect(seq)) != 1 {
		t.Fatalf("Range{} after append = (%+v, %v), want 1 frame", cur, err)
	}

	// Build a multi-chunk capture containing:
	// - sparse key (>64 matches in one chunk so the backward index buffer spills to heap)
	// - dense key (triggers the forward-scan path)
	// - sparse bus events across chunks
	capture := newTestCapture(400, 400*8)
	sparseKey := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
	denseKey := FrameKey{Bus: testBus0, ID: 0x200, Direction: DirectionReceive}
	var wantSparse []FrameEvent
	var wantDense []FrameEvent
	var wantBus0 []Event
	var chunk0End Cursor
	chunk0SparseCount := 0
	for seq := range 700 {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data, uint32(seq))
		if seq%5 == 0 {
			ev := testDataEvent(t, testBus0, sparseKey.ID, 0, data, seq, DirectionReceive)
			if err := capture.Append(ev); err != nil {
				t.Fatalf("Append sparse %d: %v", seq, err)
			}
			wantSparse = append(wantSparse, ev)
		} else if seq%10 == 1 {
			ev, err := NewErrorFrameEvent(testBus0, captureTestBase.Add(time.Duration(seq)*time.Millisecond))
			if err != nil {
				t.Fatalf("NewErrorFrameEvent: %v", err)
			}
			if err := capture.AppendEvent(ev); err != nil {
				t.Fatalf("AppendEvent %d: %v", seq, err)
			}
			wantBus0 = append(wantBus0, ev)
		} else {
			ev := testDataEvent(t, testBus0, denseKey.ID, 0, data, seq, DirectionReceive)
			if err := capture.Append(ev); err != nil {
				t.Fatalf("Append dense %d: %v", seq, err)
			}
			wantDense = append(wantDense, ev)
		}
		if len(capture.chunks) == 1 {
			chunk0End = capture.End()
			chunk0SparseCount = len(wantSparse)
		}
	}
	if len(capture.chunks) < 2 {
		t.Fatalf("chunks = %d, want at least 2", len(capture.chunks))
	}

	// Verify full streaming + deterministic repeated iteration of the same iter.Seq.
	sparseSeq, _, err := capture.Series(sparseKey, Range{})
	if err != nil {
		t.Fatalf("Series sparse: %v", err)
	}
	requireEvents(t, "sparse first pass", slices.Collect(sparseSeq), wantSparse)
	requireEvents(t, "sparse second pass", slices.Collect(sparseSeq), wantSparse)

	denseSeq, _, err := capture.Series(denseKey, Range{})
	if err != nil {
		t.Fatalf("Series dense: %v", err)
	}
	requireEvents(t, "dense first pass", slices.Collect(denseSeq), wantDense)
	requireEvents(t, "dense second pass", slices.Collect(denseSeq), wantDense)

	busSeq, _, err := capture.BusEvents(testBus0, Range{})
	if err != nil {
		t.Fatalf("BusEvents: %v", err)
	}
	requireBusEvents(t, "bus0 first pass", slices.Collect(busSeq), wantBus0)
	requireBusEvents(t, "bus0 second pass", slices.Collect(busSeq), wantBus0)

	// Verify early break and iter.Pull with early stop across all 4 iterators.
	allFramesIter, _, err := capture.Frames(Range{})
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	var brokenFrames []FrameEvent
	for ev := range allFramesIter {
		brokenFrames = append(brokenFrames, ev)
		if len(brokenFrames) == 3 {
			break
		}
	}
	if len(brokenFrames) != 3 {
		t.Fatalf("Frames early break = %d, want 3", len(brokenFrames))
	}

	allEventsIter, _, err := capture.Events(Range{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var brokenEvents []Event
	for ev := range allEventsIter {
		brokenEvents = append(brokenEvents, ev)
		if len(brokenEvents) == 3 {
			break
		}
	}
	requireBusEvents(t, "Events early break", brokenEvents, wantBus0[:3])

	brokenFrames = nil
	for ev := range sparseSeq {
		brokenFrames = append(brokenFrames, ev)
		if len(brokenFrames) == 3 {
			break
		}
	}
	requireEvents(t, "sparse early break", brokenFrames, wantSparse[:3])

	brokenFrames = nil
	for ev := range denseSeq {
		brokenFrames = append(brokenFrames, ev)
		if len(brokenFrames) == 3 {
			break
		}
	}
	requireEvents(t, "dense early break", brokenFrames, wantDense[:3])

	nextPull, stopPull := iter.Pull(busSeq)
	pulled, ok := nextPull()
	if !ok || pulled != wantBus0[0] {
		t.Fatalf("iter.Pull first bus event = (%+v, %t), want %+v", pulled, ok, wantBus0[0])
	}
	stopPull()

	// Exercise the sparse backward-skip path (state.count*4 < span, lastRecord > to)
	// and the chunk-seam start optimization (startBoundary == len(chunk0.records)-1).
	sparseSkip := newTestCapture(200, 200*8)
	var sparseEnd Cursor
	var wantSparseSkip []FrameEvent
	for seq := range 100 {
		id := denseKey.ID
		if seq == 5 || seq == 95 {
			id = sparseKey.ID
		}
		ev := testDataEvent(t, testBus0, id, 0, []byte{byte(seq), 0, 0, 0, 0, 0, 0, 0}, seq, DirectionReceive)
		if err := sparseSkip.Append(ev); err != nil {
			t.Fatalf("Append sparseSkip %d: %v", seq, err)
		}
		if seq == 5 {
			wantSparseSkip = append(wantSparseSkip, ev)
		}
		if seq == 50 {
			sparseEnd = sparseSkip.End()
		}
	}
	skipSeq, _, err := sparseSkip.Series(sparseKey, Between(Cursor{}, sparseEnd))
	if err != nil {
		t.Fatalf("Series sparse skip: %v", err)
	}
	requireEvents(t, "sparse backward skip past end", slices.Collect(skipSeq), wantSparseSkip)

	seamSeq, _, err := capture.Series(sparseKey, Between(chunk0End, capture.End()))
	if err != nil {
		t.Fatalf("Series from chunk0End seam: %v", err)
	}
	requireEvents(t, "Series from chunk0End seam", slices.Collect(seamSeq), wantSparse[chunk0SparseCount:])

	// Bounded iterators must not pin unrelated pruned, active, or later-rotated
	// chunks through capture.chunks's backing array or an unused activeView.
	gcCap := newTestCapture(2, 16)
	fdPayload := make([]byte, MaxDataLength)
	appendGCFrame := func(seq int) Cursor {
		t.Helper()
		binary.LittleEndian.PutUint32(fdPayload[:4], uint32(seq))
		ev := testDataEvent(t, testBus0, sparseKey.ID, FrameFD, fdPayload, seq, DirectionReceive)
		if err := gcCap.Append(ev); err != nil {
			t.Fatalf("gcCap Append %d: %v", seq, err)
		}
		return gcCap.End()
	}
	// Chunk 0 holds 1 small frame so we can take a 1-frame iterator before later
	// rotations grow capture.chunks.
	if err := gcCap.Append(testDataEvent(t, testBus0, sparseKey.ID, 0, []byte{1, 2, 3, 4}, 0, DirectionReceive)); err != nil {
		t.Fatalf("gcCap Append 0: %v", err)
	}
	chunk0First := gcCap.End()
	heldEarly, _, err := gcCap.Frames(Between(Cursor{}, chunk0First))
	if err != nil {
		t.Fatalf("gcCap early Frames: %v", err)
	}
	seq := 1
	for len(gcCap.chunks) < 2 {
		appendGCFrame(seq)
		seq++
	}
	chunk0Last := Cursor{
		generation: gcCap.generation,
		chunk:      gcCap.chunks[0].sequence,
		record:     uint32(len(gcCap.chunks[0].records) - 1),
	}
	for len(gcCap.chunks) < 4 {
		appendGCFrame(seq)
		seq++
	}
	chunk1Len := len(gcCap.chunks[1].records)
	chunk2Len := len(gcCap.chunks[2].records)
	chunk2Last := Cursor{
		generation: gcCap.generation,
		chunk:      gcCap.chunks[2].sequence,
		record:     uint32(chunk2Len - 1),
	}
	heldMulti, _, err := gcCap.Frames(Between(chunk0Last, chunk2Last))
	if err != nil {
		t.Fatalf("gcCap multi-chunk Frames: %v", err)
	}

	// Watch both the chunk headers and their records/payload backing arrays so
	// pinning through either capture.chunks or an unused activeView fails the test.
	chunk0Released := make(chan struct{})
	chunk0RecordsReleased := make(chan struct{})
	chunk0PayloadReleased := make(chan struct{})
	chunk3Released := make(chan struct{})
	chunk3RecordsReleased := make(chan struct{})
	chunk3PayloadReleased := make(chan struct{})
	func() {
		runtime.AddCleanup(gcCap.chunks[0], func(done chan struct{}) { close(done) }, chunk0Released)
		runtime.AddCleanup(&gcCap.chunks[0].records[0], func(done chan struct{}) { close(done) }, chunk0RecordsReleased)
		runtime.AddCleanup(&gcCap.chunks[0].payload[0], func(done chan struct{}) { close(done) }, chunk0PayloadReleased)
		runtime.AddCleanup(gcCap.chunks[3], func(done chan struct{}) { close(done) }, chunk3Released)
		runtime.AddCleanup(&gcCap.chunks[3].records[0], func(done chan struct{}) { close(done) }, chunk3RecordsReleased)
		runtime.AddCleanup(&gcCap.chunks[3].payload[0], func(done chan struct{}) { close(done) }, chunk3PayloadReleased)
	}()

	waitCollected := func(label string, ch <-chan struct{}) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			runtime.GC()
			select {
			case <-ch:
				return
			default:
				runtime.Gosched()
			}
		}
		t.Fatalf("%s was not garbage collected while unrelated iterator remained live", label)
	}

	// Prune chunk 0 and clear gcCap while both heldEarly (chunk 0) and
	// heldMulti (chunks 1..2) remain live: neither may pin unrelated chunk 3
	// or its backing arrays.
	if err := gcCap.Prune(chunk0Last); err != nil {
		t.Fatalf("gcCap Prune: %v", err)
	}
	gcCap.Clear()
	waitCollected("unrelated active chunk 3", chunk3Released)
	waitCollected("unrelated active chunk 3 records", chunk3RecordsReleased)
	waitCollected("unrelated active chunk 3 payload", chunk3PayloadReleased)
	if len(slices.Collect(heldEarly)) != 1 {
		t.Fatal("heldEarly lost its frame")
	}
	runtime.KeepAlive(heldEarly)
	heldEarly = nil

	// After dropping heldEarly, pruned chunk 0 and its backing arrays must be
	// reclaimed even while heldMulti (chunks 1..2) and gcCap remain live.
	waitCollected("pruned chunk 0", chunk0Released)
	waitCollected("pruned chunk 0 records", chunk0RecordsReleased)
	waitCollected("pruned chunk 0 payload", chunk0PayloadReleased)
	if len(slices.Collect(heldMulti)) != chunk1Len+chunk2Len {
		t.Fatal("heldMulti lost frames")
	}
	runtime.KeepAlive(heldMulti)
	runtime.KeepAlive(gcCap)

	// Re-entrancy: mutate Capture (Append, AppendEvent, Prune, Clear) and issue
	// nested reads inside a yield callback. Must not deadlock or corrupt snapshot.
	allFramesSeq, snapEnd, err := capture.Frames(Range{})
	if err != nil {
		t.Fatalf("Frames snapshot: %v", err)
	}
	expectedTotalFrames := len(wantSparse) + len(wantDense)
	seen := 0
	for range allFramesSeq {
		if seen == 0 {
			if err := capture.Append(testDataEvent(t, testBus1, 0x333, 0, []byte{0xaa}, 9000, DirectionReceive)); err != nil {
				t.Fatalf("re-entrant Append: %v", err)
			}
			if err := capture.Prune(chunk0End); err != nil {
				t.Fatalf("re-entrant Prune: %v", err)
			}
			_ = capture.Len()
			_ = capture.End()
			nestedSeq, _, err := capture.Series(FrameKey{Bus: testBus1, ID: 0x333, Direction: DirectionReceive}, Range{})
			if err != nil || len(slices.Collect(nestedSeq)) != 1 {
				t.Fatalf("re-entrant nested Series failed: %v", err)
			}
		} else if seen == 1 {
			capture.Clear()
		}
		seen++
	}
	if seen != expectedTotalFrames {
		t.Fatalf("re-entrant Frames yielded %d frames, want snapshot count %d", seen, expectedTotalFrames)
	}
	if _, _, err := capture.Frames(Since(snapEnd)); !errors.Is(err, ErrCursorOutOfRange) {
		t.Fatalf("stale cursor after re-entrant Clear = %v, want ErrCursorOutOfRange", err)
	}
}
