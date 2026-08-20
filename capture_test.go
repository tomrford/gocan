package gocan

import (
	"context"
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"testing"
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
	failAt  int
	failure error
	calls   int
	frames  []uint32
	events  []EventKind
}

func (writer *captureWriterProbe) WriteFrame(event FrameEvent) error {
	if err := writer.next(); err != nil {
		return err
	}
	writer.frames = append(writer.frames, event.Frame.ID)
	return nil
}

func (writer *captureWriterProbe) WriteEvent(event Event) error {
	if err := writer.next(); err != nil {
		return err
	}
	writer.events = append(writer.events, event.Kind)
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

// newTestCapture builds a capture whose first chunk is small enough for tests
// to seal, without the prepared successor so rotation exercises the
// synchronous fallback path.
func newTestCapture(recordCapacity, payloadCapacity int) *Capture {
	active := newCaptureChunk(recordCapacity, payloadCapacity)
	return &Capture{
		generation: captureEpochs.Add(1),
		chunks:     []*captureChunk{active},
		active:     active,
		latest:     make(map[FrameKey]FrameEvent),
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

	// The same identifier forms independent series per bus and direction.
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

	// Invalid events must be rejected without mutating the capture.
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

	// The capture must remain fully usable after Clear.
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

func TestCaptureRotationSeam(t *testing.T) {
	// Three 8-byte payloads seal the first chunk, so the fourth append rotates.
	capture := newTestCapture(4, 24)

	const total = 12
	var events, evenSeries, oddSeries []FrameEvent
	for seq := range total {
		id := uint32(0x100)
		if seq%2 == 1 {
			id = 0x200
		}
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data, uint32(seq))
		event := testDataEvent(t, testBus0, id, 0, data, seq, DirectionReceive)
		if err := capture.Append(event); err != nil {
			t.Fatalf("Append event %d: %v", seq, err)
		}
		events = append(events, event)
		if seq%2 == 0 {
			evenSeries = append(evenSeries, event)
		} else {
			oddSeries = append(oddSeries, event)
		}
	}

	if got := len(capture.chunks); got != 2 {
		t.Fatalf("capture holds %d chunks, want rotation into exactly 2", got)
	}
	requireEvents(t, "Frames across seam", capture.Frames(), events)
	requireEvents(t, "Series 0x100 across seam",
		capture.Series(FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}), evenSeries)
	requireEvents(t, "Series 0x200 across seam",
		capture.Series(FrameKey{Bus: testBus0, ID: 0x200, Direction: DirectionReceive}), oddSeries)

	latest, ok := capture.Latest(FrameKey{Bus: testBus0, ID: 0x200, Direction: DirectionReceive})
	if !ok || !eventsEqual(latest, events[total-1]) {
		t.Fatalf("Latest across seam = %+v (ok=%t), want %+v", latest, ok, events[total-1])
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

	// An empty interval is not an error: a follower that has already consumed
	// everything up to the capture end must be able to ask again.
	if got, err := capture.FramesBetween(end, end); err != nil || len(got) != 0 {
		t.Fatalf("FramesBetween over an empty interval = %d frames, %v", len(got), err)
	}
}

// TestCaptureCursorErrors covers the distinct cursor failures together with a
// valid bounded read across a chunk seam.
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

	other := NewCapture()
	if err := other.Append(testDataEvent(t, testBus0, 0x100, 0, []byte{9}, 0, DirectionReceive)); err != nil {
		t.Fatalf("append to the other capture: %v", err)
	}
	foreign := other.End()
	writer := &captureWriterProbe{failAt: -1}
	next, err := capture.WriteRecordsSince(foreign, writer)
	if !errors.Is(err, ErrCursorOutOfRange) || next != foreign ||
		len(writer.frames) != 0 || len(writer.events) != 0 {
		t.Fatalf("WriteRecordsSince with foreign cursor = %+v, %v, wrote %d records",
			next, err, len(writer.frames)+len(writer.events))
	}

	if records, err := capture.FramesBetween(end, early); !errors.Is(err, ErrCursorOutOfRange) || len(records) != 0 {
		t.Fatalf("FramesBetween over reversed range = %d records, %v", len(records), err)
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

func TestReplacementCapacities(t *testing.T) {
	tests := []struct {
		name                     string
		records, payload         int
		wantRecords, wantPayload int
	}{
		{
			name:        "classical fill keeps records at the limit",
			records:     initialCaptureChunkRecordCapacity,
			payload:     initialCaptureChunkRecordCapacity * 8,
			wantRecords: initialCaptureChunkRecordCapacity,
			wantPayload: initialCaptureChunkRecordCapacity * 8,
		},
		{
			name:        "fd fill regrows a shrunken chunk to the initial shape",
			records:     initialCaptureChunkRecordCapacity / 8,
			payload:     initialCaptureChunkRecordCapacity / 8 * MaxDataLength,
			wantRecords: initialCaptureChunkRecordCapacity,
			wantPayload: initialCaptureChunkPayloadCapacity,
		},
		{
			name:        "payload-free traffic floors the payload capacity",
			records:     1000,
			payload:     0,
			wantRecords: initialCaptureChunkRecordCapacity,
			wantPayload: minimumCaptureChunkPayloadCapacity,
		},
		{
			name:        "record-free shape keeps initial capacities",
			records:     0,
			payload:     0,
			wantRecords: initialCaptureChunkRecordCapacity,
			wantPayload: initialCaptureChunkPayloadCapacity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotRecords, gotPayload := replacementCapacities(test.records, test.payload)
			if gotRecords != test.wantRecords || gotPayload != test.wantPayload {
				t.Fatalf("replacementCapacities(%d, %d) = (%d, %d), want (%d, %d)",
					test.records, test.payload,
					gotRecords, gotPayload,
					test.wantRecords, test.wantPayload)
			}
		})
	}
}

func TestCaptureCursorIncremental(t *testing.T) {
	// Three 8-byte payloads seal the first chunk, so the increments below
	// cross a chunk seam.
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

	// Cursors are global positions: both reads observed the same tail.
	if cursorA != cursorAll {
		t.Fatalf("SeriesSince cursor %+v differs from FramesSince cursor %+v at the same tail", cursorA, cursorAll)
	}

	// At the tail: no frames, cursor unchanged.
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

	// A series read that gained nothing must still advance the cursor to the
	// global tail, so a cursor passed between read methods never replays
	// frames another read already delivered.
	appendSeq(50, 0x200)
	quiet, cursorA := readSeries("quiet", cursorA)
	if len(quiet) != 0 {
		t.Fatalf("SeriesSince returned %d frames for a series with no new traffic", len(quiet))
	}
	if replayed, _ := readFrames("quiet", cursorA); len(replayed) != 0 {
		t.Fatalf("cursor regressed: FramesSince replayed %d frames", len(replayed))
	}

	// A cursor whose history was discarded by Clear fails the read instead of
	// replaying the surviving records, and resynchronising from the zero Cursor
	// everything the capture still holds.
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

// TestCapturePrune covers the pruning lifecycle: a discard that keeps whole
// chunks, cursors that keep naming the same records across it, the cursors it
// rejects, and the release of the discarded storage, which no read can see.
func TestCapturePrune(t *testing.T) {
	// Three 8-byte payloads seal the first chunk, so the appends that follow
	// fill an active chunk that pruning must not touch.
	capture := newTestCapture(4, 24)
	key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
	var appended []FrameEvent
	var cursors []Cursor
	// The first frame carries a different ID and is never sent again, so its
	// latest entry lives in the chunk that gets discarded: the release check
	// below only holds if that index owns its frames instead of pointing at
	// chunk storage.
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

	// A cursor inside a chunk discards nothing: pruning never compacts inside
	// one.
	if err := capture.Prune(cursors[sealed-2]); err != nil || capture.Len() != len(appended) {
		t.Fatalf("Prune inside the first chunk = %v, retaining %d records", err, capture.Len())
	}

	if err := capture.Prune(cursors[sealed-1]); err != nil {
		t.Fatalf("Prune at the chunk seam: %v", err)
	}
	if capture.Len() != len(appended)-sealed {
		t.Fatalf("capture retains %d records, want %d", capture.Len(), len(appended)-sealed)
	}

	// Cursors keep their identity across the discard: the retained one still
	// names the same record, the prune-point cursor still starts the rest, and
	// each starting point reads the retained history.
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

	// A follow-and-prune loop continues from the prune point: Next and a
	// bounded read both see the first retained record.
	event, _, err := capture.Next(context.Background(), key, cursors[sealed-1])
	if err != nil || !eventsEqual(event, appended[sealed]) {
		t.Fatalf("Next from the prune-point cursor = (%+v, %v), want %+v", event, err, appended[sealed])
	}
	between, err := capture.FramesBetween(cursors[sealed-1], capture.End())
	if err != nil {
		t.Fatalf("FramesBetween from the prune-point cursor: %v", err)
	}
	requireEvents(t, "FramesBetween from the prune-point cursor", between, appended[sealed:])

	// A cursor into the discarded chunk is rejected, never quietly replaced.
	stale, returned, err := capture.FramesSince(cursors[0])
	if !errors.Is(err, ErrCursorOutOfRange) || len(stale) != 0 || returned != cursors[0] {
		t.Fatalf("FramesSince a discarded cursor = %d frames, cursor %+v, %v", len(stale), returned, err)
	}
	if err := capture.Prune(cursors[0]); !errors.Is(err, ErrCursorOutOfRange) {
		t.Fatalf("Prune with a discarded cursor = %v, want ErrCursorOutOfRange", err)
	}

	// Latest owns its frames, so pruning cannot take the newest frame away,
	// nor the last frame of an ID whose records were discarded entirely.
	if latest, ok := capture.Latest(key); !ok || !eventsEqual(latest, appended[len(appended)-1]) {
		t.Fatalf("Latest after pruning = %+v (ok=%t), want the newest appended frame", latest, ok)
	}
	if latest, ok := capture.Latest(quiet); !ok || !eventsEqual(latest, appended[0]) {
		t.Fatalf("Latest of a discarded ID = %+v (ok=%t), want its last frame", latest, ok)
	}

	// The chunk being appended to is never discarded, so pruning at the
	// frontier still keeps the newest records.
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

// TestCaptureOldestNewest covers picking the earliest and latest of a set of
// cursors: argument order, and both helpers failing when any member cannot
// place, which is the signal a prune set has lost a reader.
func TestCaptureOldestNewest(t *testing.T) {
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

	if oldest, err := capture.Oldest(); !errors.Is(err, ErrCursorOutOfRange) || oldest != (Cursor{}) {
		t.Fatalf("Oldest with no cursors = (%+v, %v), want ErrCursorOutOfRange", oldest, err)
	}
	if newest, err := capture.Newest(); !errors.Is(err, ErrCursorOutOfRange) || newest != (Cursor{}) {
		t.Fatalf("Newest with no cursors = (%+v, %v), want ErrCursorOutOfRange", newest, err)
	}

	early, late := cursors[1], cursors[len(cursors)-1]
	oldest, err := capture.Oldest(late, early, late)
	if err != nil || oldest != early {
		t.Fatalf("Oldest of placeable cursors = (%+v, %v), want %+v", oldest, err, early)
	}
	newest, err := capture.Newest(early, late, early)
	if err != nil || newest != late {
		t.Fatalf("Newest of placeable cursors = (%+v, %v), want %+v", newest, err, late)
	}

	zero, err := capture.Oldest(Cursor{}, late)
	if err != nil || zero != (Cursor{}) {
		t.Fatalf("Oldest with the zero Cursor = (%+v, %v), want the zero Cursor", zero, err)
	}

	if err := capture.Prune(cursors[3]); err != nil {
		t.Fatalf("Prune at the chunk seam: %v", err)
	}

	if oldest, err = capture.Oldest(cursors[0], cursors[5], cursors[6]); !errors.Is(err, ErrCursorOutOfRange) || oldest != (Cursor{}) {
		t.Fatalf("Oldest with a discarded cursor = (%+v, %v), want ErrCursorOutOfRange", oldest, err)
	}
	if newest, err = capture.Newest(cursors[0], cursors[5], cursors[6]); !errors.Is(err, ErrCursorOutOfRange) || newest != (Cursor{}) {
		t.Fatalf("Newest with a discarded cursor = (%+v, %v), want ErrCursorOutOfRange", newest, err)
	}

	other := NewCapture()
	if err := other.Append(testDataEvent(t, testBus0, 0x100, 0, []byte{9}, 0, DirectionReceive)); err != nil {
		t.Fatalf("append to the other capture: %v", err)
	}
	if oldest, err = capture.Oldest(other.End(), cursors[6]); !errors.Is(err, ErrCursorOutOfRange) || oldest != (Cursor{}) {
		t.Fatalf("Oldest with a foreign cursor = (%+v, %v), want ErrCursorOutOfRange", oldest, err)
	}
	if newest, err = capture.Newest(other.End(), cursors[6]); !errors.Is(err, ErrCursorOutOfRange) || newest != (Cursor{}) {
		t.Fatalf("Newest with a foreign cursor = (%+v, %v), want ErrCursorOutOfRange", newest, err)
	}

	past := capture.End()
	past.chunk += 8
	if oldest, err = capture.Oldest(cursors[5], past); !errors.Is(err, ErrCursorOutOfRange) || oldest != (Cursor{}) {
		t.Fatalf("Oldest with a cursor past the end = (%+v, %v), want ErrCursorOutOfRange", oldest, err)
	}
	if newest, err = capture.Newest(cursors[5], past); !errors.Is(err, ErrCursorOutOfRange) || newest != (Cursor{}) {
		t.Fatalf("Newest with a cursor past the end = (%+v, %v), want ErrCursorOutOfRange", newest, err)
	}

	oldest, err = capture.Oldest(cursors[5], cursors[6])
	if err != nil || oldest != cursors[5] {
		t.Fatalf("Oldest of retained cursors after prune = (%+v, %v), want %+v", oldest, err, cursors[5])
	}
	newest, err = capture.Newest(cursors[5], cursors[6])
	if err != nil || newest != cursors[6] {
		t.Fatalf("Newest of retained cursors after prune = (%+v, %v), want %+v", newest, err, cursors[6])
	}
}

func TestCaptureNext(t *testing.T) {
	capture := newTestCapture(8, 64)
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

	registrationDeadline := time.Now().Add(time.Second)
	for {
		capture.mu.RLock()
		waiterCount := 0
		if waiter := capture.waiters[key]; waiter != nil {
			waiterCount = waiter.count
		}
		capture.mu.RUnlock()
		if waiterCount == 2 {
			break
		}
		if time.Now().After(registrationDeadline) {
			t.Fatal("Next waiters did not register")
		}
		runtime.Gosched()
	}

	third := testDataEvent(t, testBus0, 0x100, 0, []byte{3}, 2, DirectionReceive)
	if err := capture.Append(third); err != nil {
		t.Fatalf("Append third: %v", err)
	}
	for range 2 {
		var result nextResult
		select {
		case result = <-results:
		case <-time.After(time.Second):
			t.Fatal("Next waiter did not wake")
		}
		if result.err != nil || !eventsEqual(result.event, third) {
			t.Fatalf("waiting Next = (%+v, %v), want third event", result.event, result.err)
		}
		if result.cursor != capture.End() {
			t.Fatalf("waiting Next cursor = %+v, want current end %+v", result.cursor, capture.End())
		}
	}
}

func TestCaptureNextWalksAcrossRotation(t *testing.T) {
	// Three 8-byte payloads seal the first chunk, so the walk crosses a seam.
	capture := newTestCapture(4, 24)
	key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}

	var want []FrameEvent
	for seq := range 12 {
		id := uint32(0x100)
		if seq%2 == 1 {
			id = 0x200
		}
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data, uint32(seq))
		event := testDataEvent(t, testBus0, id, 0, data, seq, DirectionReceive)
		if err := capture.Append(event); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
		if id == 0x100 {
			want = append(want, event)
		}
	}
	if len(capture.chunks) < 2 {
		t.Fatalf("capture holds %d chunks, want the walk to span a rotation", len(capture.chunks))
	}

	var got []FrameEvent
	var cursor Cursor
	for range len(want) {
		event, next, err := capture.Next(context.Background(), key, cursor)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, event)
		cursor = next
	}
	requireEvents(t, "Next walk across seam", got, want)

	// The series is exhausted: one more Next must wait, not return a frame,
	// and cancellation must not leak its waiter registration.
	expired, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := capture.Next(expired, key, cursor); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next past the tail returned %v, want context.DeadlineExceeded", err)
	}
	capture.mu.RLock()
	leaked := len(capture.waiters)
	capture.mu.RUnlock()
	if leaked != 0 {
		t.Fatalf("%d waiter entries remain after cancellation", leaked)
	}
}

// waitForCaptureWaiter blocks until one Next has parked on key.
func waitForCaptureWaiter(t *testing.T, capture *Capture, key FrameKey) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		capture.mu.RLock()
		parked := capture.waiters[key] != nil
		capture.mu.RUnlock()
		if parked {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("no Next parked on the key")
}

// TestCaptureNextAcrossClear covers the registration boundary: a Next that has
// parked no longer depends on its input cursor and survives Clear, while a new
// call carrying that old-generation cursor still fails.
func TestCaptureNextAcrossClear(t *testing.T) {
	capture := newTestCapture(8, 64)
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
	waitForCaptureWaiter(t, capture, key)
	capture.Clear()

	select {
	case got := <-pending:
		t.Fatalf("registered Next returned (%+v, %v) during Clear", got.event, got.err)
	default:
	}

	if err := capture.Append(testDataEvent(t, testBus0, other.ID, 0, []byte{2}, 1, DirectionReceive)); err != nil {
		t.Fatalf("append unrelated frame: %v", err)
	}
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
}

// TestCaptureNextAcrossPrune covers Next already parked when Prune discards a
// sealed chunk. Both calls have exhausted a valid snapshot, so their input
// cursors are no longer needed and the next matching append wakes them. A new
// call from the earlier discarded cursor still fails.
func TestCaptureNextAcrossPrune(t *testing.T) {
	// Three 8-byte payloads seal the first chunk. Only the first record matches
	// the followed key, so both Next calls wait: one from that record, one from
	// the prune point at the sealed tail.
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

	deadline := time.Now().Add(2 * time.Second)
	for {
		capture.mu.RLock()
		parked := 0
		if waiter := capture.waiters[key]; waiter != nil {
			parked = waiter.count
		}
		capture.mu.RUnlock()
		if parked == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Next waiters did not register")
		}
		runtime.Gosched()
	}

	if err := capture.Prune(sealed); err != nil {
		t.Fatalf("Prune at the chunk seam: %v", err)
	}

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

	capture.mu.RLock()
	leaked := len(capture.waiters)
	capture.mu.RUnlock()
	if leaked != 0 {
		t.Fatalf("%d waiter entries remain after the prune/wait/append lifecycle", leaked)
	}
}

// TestCaptureConcurrentClear hammers appends, reads, and Clear together. Run
// with -race: readers copy without the lock, so this validates that Clear
// replaces storage instead of mutating what a reader may still hold.
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

			// Clear invalidates both cursors, so the reader resynchronises the
			// way a follower does: it reports nothing and starts again from the
			// oldest retained record.
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

	// A follower on the blocking path meets Clear at every point of the walk,
	// including the window between the walk and the waiter registration, and
	// must never leave a waiter behind.
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

	capture.mu.RLock()
	leaked := len(capture.waiters)
	capture.mu.RUnlock()
	if leaked != 0 {
		t.Fatalf("%d waiter entries remain after Clear churn", leaked)
	}

	// Whatever survived the final Clear must be, per bus, a consecutive run of
	// the most recently appended sequence numbers, and the total must agree.
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

// TestCaptureConcurrent hammers concurrent appends and reads across a chunk
// rotation. Run with -race.
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

	// A live tailer must observe every frame of its series exactly once.
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

	// A blocking tailer must observe every frame of its series exactly once,
	// in order, sleeping on the waiter path whenever it outruns its writer.
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
