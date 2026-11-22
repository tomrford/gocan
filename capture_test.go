package gocan

import (
	"context"
	"encoding/binary"
	"errors"
	"runtime"
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
		if _, err := capture.WriteRecordsSince(Cursor{}, writer); err != nil {
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
	active := newCaptureChunk(recordCapacity, payloadCapacity)
	return &Capture{
		generation:   captureEpochs.Add(1),
		chunks:       []*captureChunk{active},
		active:       active,
		latest:       make(map[FrameKey]FrameEvent),
		latestStates: make(map[BusID]Event),
	}
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
	requireEvents(t, "Frames", capture.Frames(), events)

	requireEvents(t, "Series bus0/0x100 RX",
		capture.Series(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}),
		[]FrameEvent{events[0]})
	requireEvents(t, "Series bus0/0x100 TX",
		capture.Series(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionTransmit}),
		[]FrameEvent{events[2]})
	requireEvents(t, "Series bus1/0x100",
		capture.Series(FrameKey{Bus: testBus1, ID: 0x100, Direction: DirectionReceive}),
		[]FrameEvent{events[1]})
	requireEvents(t, "Series extended",
		capture.Series(FrameKey{Bus: testBus0, ID: 0x1abcde, Direction: DirectionReceive, Extended: true}),
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
	if got := capture.Frames(); len(got) != 0 {
		t.Fatalf("Frames after Clear returned %d events", len(got))
	}
	if _, ok := capture.Latest(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}); ok {
		t.Fatal("Latest found a key after Clear")
	}
	if got := capture.Series(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}); len(got) != 0 {
		t.Fatalf("Series after Clear returned %d events", len(got))
	}

	revived := testDataEvent(t, testBus0, 0x100, 0, []byte{9}, 6, DirectionReceive)
	if err := capture.Append(revived); err != nil {
		t.Fatalf("Append after Clear: %v", err)
	}
	requireEvents(t, "Series after Clear",
		capture.Series(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}),
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
	requireEvents(t, "all frames", capture.Frames(), []FrameEvent{frame0, frame1, frame2})
	requireBusEvents(t, "all events", capture.Events(), []Event{state0, error1, overrun0, state1})
	requireBusEvents(t, "bus 0 events", capture.BusEvents(testBus0), []Event{state0, overrun0})
	requireBusEvents(t, "bus 1 events", capture.BusEvents(testBus1), []Event{error1, state1})

	frames, frameCursor, err := capture.FramesSince(cursor)
	if err != nil {
		t.Fatalf("FramesSince: %v", err)
	}
	requireEvents(t, "later frames", frames, []FrameEvent{frame2})
	events, eventCursor, err := capture.EventsSince(cursor)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	requireBusEvents(t, "later events", events, []Event{error1, overrun0, state1})
	bus0, bus0Cursor, err := capture.BusEventsSince(testBus0, cursor)
	if err != nil {
		t.Fatalf("BusEventsSince bus 0: %v", err)
	}
	requireBusEvents(t, "later bus 0 events", bus0, []Event{overrun0})
	bus1, bus1Cursor, err := capture.BusEventsSince(testBus1, cursor)
	if err != nil {
		t.Fatalf("BusEventsSince bus 1: %v", err)
	}
	requireBusEvents(t, "later bus 1 events", bus1, []Event{error1, state1})
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
	for _, event := range capture.BusEvents(testBus0) {
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

	between, err := capture.FramesBetween(start, end)
	if err != nil {
		t.Fatalf("FramesBetween: %v", err)
	}
	requireEvents(t, "FramesBetween", between, frames[1:2])

	if got, err := capture.FramesBetween(end, end); err != nil || len(got) != 0 {
		t.Fatalf("FramesBetween over an empty interval = %d frames, %v", len(got), err)
	}
	if got, err := capture.SeriesBetween(frames[0].Key(), Cursor{}, Cursor{}); err != nil || len(got) != 0 {
		t.Fatalf("SeriesBetween over zero interval = %d frames, %v", len(got), err)
	}
	if got, err := capture.BusEventsBetween(testBus0, Cursor{}, Cursor{}); err != nil || len(got) != 0 {
		t.Fatalf("BusEventsBetween over zero interval = %d events, %v", len(got), err)
	}

	// Single-chunk indexed between: matching records before start, inside (start, end], and after end.
	singleSeries, err := capture.SeriesBetween(frames[0].Key(), start, end)
	if err != nil {
		t.Fatalf("SeriesBetween single chunk: %v", err)
	}
	requireEvents(t, "SeriesBetween single chunk", singleSeries, frames[1:2])

	// Multi-chunk indexed SeriesBetween and BusEventsBetween across sealed,
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
	// so the bounded backward skip falls back to scanning the narrow tail range.
	for seq := 204; seq < 240; seq++ {
		appendFrame(keyA.ID, seq)
		appendErr(testBus0, seq)
	}

	gotSeries, err := multi.SeriesBetween(keyA, rangeStart, rangeEnd)
	if err != nil {
		t.Fatalf("SeriesBetween multi-chunk: %v", err)
	}
	requireEvents(t, "SeriesBetween multi-chunk", gotSeries, []FrameEvent{wantA1, wantA2})

	gotBusEvents, err := multi.BusEventsBetween(testBus0, rangeStart, rangeEnd)
	if err != nil {
		t.Fatalf("BusEventsBetween multi-chunk: %v", err)
	}
	requireBusEvents(t, "BusEventsBetween multi-chunk", gotBusEvents, []Event{wantEv0, wantEv1})

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
		if got, err := multi.FramesBetween(pair[0], pair[1]); err != nil || len(got) != 0 {
			t.Fatalf("FramesBetween(%+v, %+v) = %d, %v", pair[0], pair[1], len(got), err)
		}
		if got, err := multi.SeriesBetween(keyA, pair[0], pair[1]); err != nil || len(got) != 0 {
			t.Fatalf("SeriesBetween(%+v, %+v) = %d, %v", pair[0], pair[1], len(got), err)
		}
		if got, err := multi.BusEventsBetween(testBus0, pair[0], pair[1]); err != nil || len(got) != 0 {
			t.Fatalf("BusEventsBetween(%+v, %+v) = %d, %v", pair[0], pair[1], len(got), err)
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

	spanning, err := capture.FramesBetween(early, end)
	if err != nil {
		t.Fatalf("FramesBetween across a seam: %v", err)
	}
	requireEvents(t, "FramesBetween across a seam", spanning, appended[1:])
	series, err := capture.SeriesBetween(key, early, end)
	if err != nil {
		t.Fatalf("SeriesBetween across a seam: %v", err)
	}
	requireEvents(t, "SeriesBetween across a seam", series, appended[1:])

	beyondSealed := Cursor{
		generation: early.generation,
		chunk:      early.chunk,
		record:     uint32(len(capture.chunks[0].records)),
	}
	if _, _, err := capture.FramesSince(beyondSealed); err == nil {
		t.Fatal("FramesSince accepted cursor beyond sealed chunk length")
	} else {
		requireRejectedCursor(t, err, beyondSealed)
	}
	if _, err := capture.SeriesBetween(key, beyondSealed, end); err == nil {
		t.Fatal("SeriesBetween accepted start cursor beyond sealed chunk length")
	} else {
		requireRejectedCursor(t, err, beyondSealed)
	}

	other := NewCapture()
	if err := other.Append(testDataEvent(t, testBus0, 0x100, 0, []byte{9}, 0, DirectionReceive)); err != nil {
		t.Fatalf("append to the other capture: %v", err)
	}
	foreign := other.End()
	if records, err := capture.FramesBetween(end, early); !errors.Is(err, ErrCursorOutOfRange) || len(records) != 0 {
		t.Fatalf("FramesBetween over reversed range = %d records, %v", len(records), err)
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
			read func() (int, Cursor, error)
		}{
			{"FramesSince", func() (int, Cursor, error) {
				records, next, err := capture.FramesSince(cursor)
				return len(records), next, err
			}},
			{"EventsSince", func() (int, Cursor, error) {
				records, next, err := capture.EventsSince(cursor)
				return len(records), next, err
			}},
			{"SeriesSince", func() (int, Cursor, error) {
				records, next, err := capture.SeriesSince(key, cursor)
				return len(records), next, err
			}},
			{"BusEventsSince", func() (int, Cursor, error) {
				records, next, err := capture.BusEventsSince(testBus0, cursor)
				return len(records), next, err
			}},
			{"WriteRecordsSince", func() (int, Cursor, error) {
				next, err := capture.WriteRecordsSince(cursor, writer)
				return len(writer.frames) + len(writer.events), next, err
			}},
		}
		for _, read := range since {
			count, next, err := read.read()
			requireRejectedCursor(t, err, cursor)
			if count != 0 || next != cursor {
				t.Fatalf("%s with rejected cursor = %d records, %+v, want no records and %+v", read.name, count, next, cursor)
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
			read func(Cursor, Cursor) (int, error)
		}{
			{"FramesBetween", func(start, end Cursor) (int, error) {
				records, err := capture.FramesBetween(start, end)
				return len(records), err
			}},
			{"EventsBetween", func(start, end Cursor) (int, error) {
				records, err := capture.EventsBetween(start, end)
				return len(records), err
			}},
			{"SeriesBetween", func(start, end Cursor) (int, error) {
				records, err := capture.SeriesBetween(key, start, end)
				return len(records), err
			}},
			{"BusEventsBetween", func(start, end Cursor) (int, error) {
				records, err := capture.BusEventsBetween(testBus0, start, end)
				return len(records), err
			}},
			{"WriteRecordsBetween", func(start, end Cursor) (int, error) {
				next, err := capture.WriteRecordsBetween(start, end, writer)
				if next != start {
					t.Fatalf("failed WriteRecordsBetween cursor = %+v, want %+v", next, start)
				}
				return len(writer.frames) + len(writer.events), err
			}},
		}
		for _, read := range between {
			for _, bounds := range [][2]Cursor{{cursor, capture.End()}, {Cursor{}, cursor}} {
				count, err := read.read(bounds[0], bounds[1])
				requireRejectedCursor(t, err, cursor)
				if count != 0 {
					t.Fatalf("%s with rejected cursor returned %d records", read.name, count)
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
	written, err := capture.WriteRecordsSince(Cursor{}, first)
	var recordError *RecordWriteError
	if !errors.As(err, &recordError) {
		t.Fatalf("WriteRecordsSince error = %v, want *RecordWriteError", err)
	}
	if !errors.Is(err, writeFailure) {
		t.Fatalf("WriteRecordsSince error = %v, want wrapped writer failure", err)
	}
	if len(first.frames) != 1 || first.frames[0] != frame0.Frame.ID ||
		len(first.events) != 1 || first.events[0] != state.Kind {
		t.Fatalf("records before failure = frames %v events %v", first.frames, first.events)
	}
	if written == capture.End() || written == recordError.Cursor {
		t.Fatal("failure did not distinguish the last written and failed record cursors")
	}

	retry := &captureWriterProbe{failAt: -1}
	next, err := capture.WriteRecordsSince(written, retry)
	if err != nil {
		t.Fatalf("retry WriteRecordsSince: %v", err)
	}
	if len(retry.frames) != 1 || retry.frames[0] != frame1.Frame.ID ||
		len(retry.events) != 1 || retry.events[0] != errorEvent.Kind {
		t.Fatalf("retried records = frames %v events %v", retry.frames, retry.events)
	}
	if next != capture.End() {
		t.Fatal("successful retry did not advance to the capture end")
	}

	skip := &captureWriterProbe{failAt: -1}
	if _, err := capture.WriteRecordsSince(recordError.Cursor, skip); err != nil {
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
		series, next, err := capture.SeriesSince(keyA, cursor)
		if err != nil {
			t.Fatalf("%s SeriesSince: %v", what, err)
		}
		return series, next
	}
	readFrames := func(what string, cursor Cursor) ([]FrameEvent, Cursor) {
		t.Helper()
		all, next, err := capture.FramesSince(cursor)
		if err != nil {
			t.Fatalf("%s FramesSince: %v", what, err)
		}
		return all, next
	}

	seriesA, cursorA := readSeries("initial", Cursor{})
	requireEvents(t, "initial SeriesSince", seriesA, initialA)
	frames, cursorAll := readFrames("initial", Cursor{})
	requireEvents(t, "initial FramesSince", frames, initialAll)

	if cursorA != cursorAll {
		t.Fatalf("SeriesSince cursor %+v differs from FramesSince cursor %+v at the same tail", cursorA, cursorAll)
	}

	if frames, cursor := readSeries("tail", cursorA); len(frames) != 0 || cursor != cursorA {
		t.Fatalf("SeriesSince at tail returned %d frames (cursor changed: %t)", len(frames), cursor != cursorA)
	}
	if frames, cursor := readFrames("tail", cursorAll); len(frames) != 0 || cursor != cursorAll {
		t.Fatalf("FramesSince at tail returned %d frames (cursor changed: %t)", len(frames), cursor != cursorAll)
	}

	freshAll, freshA := appendBatch(4, 12)
	if got := len(capture.chunks); got < 2 {
		t.Fatalf("capture holds %d chunks, want the increments to span a rotation", got)
	}

	seriesA, cursorA = readSeries("incremental", cursorA)
	requireEvents(t, "incremental SeriesSince across seam", seriesA, freshA)
	frames, _ = readFrames("incremental", cursorAll)
	requireEvents(t, "incremental FramesSince across seam", frames, freshAll)

	appendSeq(50, 0x200)
	quiet, cursorA := readSeries("quiet", cursorA)
	if len(quiet) != 0 {
		t.Fatalf("SeriesSince returned %d frames for a series with no new traffic", len(quiet))
	}
	if replayed, _ := readFrames("quiet", cursorA); len(replayed) != 0 {
		t.Fatalf("cursor regressed: FramesSince replayed %d frames", len(replayed))
	}

	capture.Clear()
	revived := appendSeq(100, 0x100)
	stale, returned, err := capture.SeriesSince(keyA, cursorA)
	if !errors.Is(err, ErrCursorOutOfRange) {
		t.Fatalf("SeriesSince after Clear error = %v, want ErrCursorOutOfRange", err)
	}
	if len(stale) != 0 || returned != cursorA {
		t.Fatalf("stale SeriesSince returned %d frames and cursor %+v", len(stale), returned)
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
		frames, _, err := capture.FramesSince(from.cursor)
		if err != nil {
			t.Fatalf("FramesSince %s after pruning: %v", from.label, err)
		}
		requireEvents(t, "FramesSince "+from.label+" after pruning", frames, from.want)
	}

	event, _, err := capture.Next(context.Background(), key, cursors[sealed-1])
	if err != nil || !eventsEqual(event, appended[sealed]) {
		t.Fatalf("Next from the prune-point cursor = (%+v, %v), want %+v", event, err, appended[sealed])
	}
	between, err := capture.FramesBetween(cursors[sealed-1], capture.End())
	if err != nil {
		t.Fatalf("FramesBetween from the prune-point cursor: %v", err)
	}
	requireEvents(t, "FramesBetween from the prune-point cursor", between, appended[sealed:])

	stale, returned, err := capture.FramesSince(cursors[0])
	if !errors.Is(err, ErrCursorOutOfRange) || len(stale) != 0 || returned != cursors[0] {
		t.Fatalf("FramesSince a discarded cursor = %d frames, cursor %+v, %v", len(stale), returned, err)
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
			capture.Frames()
			capture.Series(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive})
			capture.Latest(FrameKey{Bus: testBus1, ID: 0x100, Direction: DirectionReceive})

			_, next, err := capture.EventsSince(eventsCursor)
			if err != nil && !errors.Is(err, ErrCursorOutOfRange) {
				t.Errorf("EventsSince: %v", err)
				return
			}
			eventsCursor = next
			if err != nil {
				eventsCursor = Cursor{}
			}
			_, next, err = capture.BusEventsSince(testBus0, busEventsCursor)
			if err != nil && !errors.Is(err, ErrCursorOutOfRange) {
				t.Errorf("BusEventsSince: %v", err)
				return
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
		series := capture.Series(key)
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
	events := capture.BusEvents(testBus0)
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
				capture.Frames()
				capture.Series(key)
				capture.Latest(FrameKey{Bus: testBus1, ID: 0x100, Direction: DirectionReceive})
				capture.Events()
				capture.BusEvents(testBus0)
				capture.Len()
				end := capture.End()
				_, framesErr := capture.FramesBetween(Cursor{}, end)
				_, eventsErr := capture.EventsBetween(Cursor{}, end)
				_, seriesErr := capture.SeriesBetween(key, Cursor{}, end)
				_, busEventsErr := capture.BusEventsBetween(testBus0, Cursor{}, end)
				if err := errors.Join(framesErr, eventsErr, seriesErr, busEventsErr); err != nil {
					t.Errorf("bounded read of a live capture: %v", err)
					return
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
			frames, next, err := capture.SeriesSince(key, cursor)
			if err != nil {
				t.Errorf("tailing SeriesSince: %v", err)
				return nil, cursor, false
			}
			return frames, next, true
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
			events, next, err := capture.EventsSince(cursor)
			if err != nil {
				t.Errorf("tailing EventsSince: %v", err)
				return nil, cursor, false
			}
			return events, next, true
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
			events, next, err := capture.BusEventsSince(testBus0, cursor)
			if err != nil {
				t.Errorf("tailing BusEventsSince: %v", err)
				return nil, cursor, false
			}
			return events, next, true
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
		capture.Series(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}))
	requireEvents(t, "Next-tailed series", nextTailed,
		capture.Series(FrameKey{Bus: testBus1, ID: 0x100, Direction: DirectionReceive}))
	requireBusEvents(t, "tailed events", eventsTailed, capture.Events())
	requireBusEvents(t, "tailed bus events", busEventsTailed, capture.BusEvents(testBus0))

	if got := capture.Len(); got != (writers+1)*perWriter {
		t.Fatalf("Len() = %d, want %d", got, (writers+1)*perWriter)
	}
	for w := range writers {
		key := FrameKey{Bus: BusID(w + 1), ID: 0x100, Direction: DirectionReceive}
		series := capture.Series(key)
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
	series := capture.Series(key)
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
