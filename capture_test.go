package gocan

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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
		latest:     make(map[FrameKey]captureLocation),
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

// TestCaptureBetweenAcrossSeam bounds an interval whose start and end lie in
// different chunks, so the read must clip the last chunk without dropping the
// records of the chunks in between.
func TestCaptureBetweenAcrossSeam(t *testing.T) {
	// Three 8-byte payloads seal the first chunk, so the appends below rotate.
	capture := newTestCapture(4, 24)

	const total = 12
	var frames []FrameEvent
	var start, end Cursor
	for seq := range total {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data, uint32(seq))
		event := testDataEvent(t, testBus0, 0x100, 0, data, seq, DirectionReceive)
		if err := capture.Append(event); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
		frames = append(frames, event)
		switch seq {
		case 1:
			start = capture.End()
		case 8:
			end = capture.End()
		}
	}
	if start.chunk == end.chunk {
		t.Fatalf("interval stayed inside chunk %d, want it to cross a seam", start.chunk)
	}

	between, err := capture.FramesBetween(start, end)
	if err != nil {
		t.Fatalf("FramesBetween: %v", err)
	}
	requireEvents(t, "FramesBetween across seam", between, frames[2:9])

	series, err := capture.SeriesBetween(
		FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive},
		start,
		end,
	)
	if err != nil {
		t.Fatalf("SeriesBetween: %v", err)
	}
	requireEvents(t, "SeriesBetween across seam", series, frames[2:9])

	writer := &captureWriterProbe{failAt: -1}
	if next, err := capture.WriteRecordsBetween(start, end, writer); err != nil || next != end {
		t.Fatalf("WriteRecordsBetween = %+v, %v, want the end cursor and no error", next, err)
	}
	if len(writer.frames) != 7 {
		t.Fatalf("WriteRecordsBetween wrote %d records, want 7", len(writer.frames))
	}
}

// cursorRead is one cursor-consuming read reduced to a record count, the
// cursor it hands back, and its error, so every read family can be held to the
// same failure contract.
type cursorRead struct {
	name string
	read func(cursor Cursor) (int, Cursor, error)
}

// rangeRead is one bounded read reduced to a record count and its error.
type rangeRead struct {
	name string
	read func(start, end Cursor) (int, error)
}

func cursorReads(capture *Capture, key FrameKey) []cursorRead {
	return []cursorRead{
		{"WriteRecordsSince", func(cursor Cursor) (int, Cursor, error) {
			writer := &captureWriterProbe{failAt: -1}
			next, err := capture.WriteRecordsSince(cursor, writer)
			return len(writer.frames) + len(writer.events), next, err
		}},
		{"FramesSince", func(cursor Cursor) (int, Cursor, error) {
			frames, next, err := capture.FramesSince(cursor)
			return len(frames), next, err
		}},
		{"EventsSince", func(cursor Cursor) (int, Cursor, error) {
			events, next, err := capture.EventsSince(cursor)
			return len(events), next, err
		}},
		{"SeriesSince", func(cursor Cursor) (int, Cursor, error) {
			frames, next, err := capture.SeriesSince(key, cursor)
			return len(frames), next, err
		}},
		{"BusEventsSince", func(cursor Cursor) (int, Cursor, error) {
			events, next, err := capture.BusEventsSince(key.Bus, cursor)
			return len(events), next, err
		}},
		{"Next", func(cursor Cursor) (int, Cursor, error) {
			// A rejected cursor must fail before Next reaches its waiter, so a
			// short context is enough to prove the read does not block.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, next, err := capture.Next(ctx, key, cursor)
			if err != nil {
				return 0, next, err
			}
			return 1, next, nil
		}},
	}
}

func rangeReads(capture *Capture, key FrameKey) []rangeRead {
	return []rangeRead{
		{"WriteRecordsBetween", func(start, end Cursor) (int, error) {
			writer := &captureWriterProbe{failAt: -1}
			next, err := capture.WriteRecordsBetween(start, end, writer)
			if err != nil && next != start {
				return 0, fmt.Errorf("returned cursor %+v, want start %+v: %w", next, start, err)
			}
			return len(writer.frames) + len(writer.events), err
		}},
		{"FramesBetween", func(start, end Cursor) (int, error) {
			frames, err := capture.FramesBetween(start, end)
			return len(frames), err
		}},
		{"EventsBetween", func(start, end Cursor) (int, error) {
			events, err := capture.EventsBetween(start, end)
			return len(events), err
		}},
		{"SeriesBetween", func(start, end Cursor) (int, error) {
			frames, err := capture.SeriesBetween(key, start, end)
			return len(frames), err
		}},
		{"BusEventsBetween", func(start, end Cursor) (int, error) {
			events, err := capture.BusEventsBetween(key.Bus, start, end)
			return len(events), err
		}},
	}
}

// TestCaptureCursorErrors holds every cursor-consuming read to one contract: a
// cursor the capture cannot place fails the read, yields no records, and hands
// back the cursor the caller passed in, so a retry loop can neither replay nor
// skip history.
func TestCaptureCursorErrors(t *testing.T) {
	// Three 8-byte payloads seal the first chunk, so the fixture spans a seam
	// and a rejected cursor can name a sealed chunk as well as the active one.
	capture := newTestCapture(4, 24)
	key := FrameKey{Bus: testBus0, ID: 0x100, Direction: DirectionReceive}
	appendFrame := func(seq int) {
		t.Helper()
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data, uint32(seq))
		if err := capture.Append(testDataEvent(t, testBus0, 0x100, 0, data, seq, DirectionReceive)); err != nil {
			t.Fatalf("Append %d: %v", seq, err)
		}
	}
	appendFrame(0)
	early := capture.End()
	state, err := NewControllerStateEvent(testBus0, captureTestBase.Add(time.Millisecond), ControllerActive, 0, 0, true)
	if err != nil {
		t.Fatalf("NewControllerStateEvent: %v", err)
	}
	if err := capture.AppendEvent(state); err != nil {
		t.Fatalf("append event: %v", err)
	}
	for seq := 2; seq < 6; seq++ {
		appendFrame(seq)
	}
	end := capture.End()
	if end.chunk == 0 {
		t.Fatalf("fixture stayed inside one chunk, want it to rotate")
	}

	reads := cursorReads(capture, key)
	bounded := rangeReads(capture, key)

	requireRejected := func(t *testing.T, cursor Cursor, want error) {
		t.Helper()
		for _, read := range reads {
			records, next, err := read.read(cursor)
			if !errors.Is(err, want) {
				t.Errorf("%s error = %v, want %v", read.name, err, want)
			}
			if records != 0 {
				t.Errorf("%s returned %d records for a rejected cursor", read.name, records)
			}
			if next != cursor {
				t.Errorf("%s returned cursor %+v, want the rejected cursor back", read.name, next)
			}
		}
		for _, read := range bounded {
			if records, err := read.read(cursor, end); !errors.Is(err, want) || records != 0 {
				t.Errorf("%s with a rejected start = %d records, %v, want %v", read.name, records, err, want)
			}
			// A rejected end must fail behind both a zero and a placeable start.
			for _, start := range []Cursor{{}, early} {
				if records, err := read.read(start, cursor); !errors.Is(err, want) || records != 0 {
					t.Errorf("%s with a rejected end after start %+v = %d records, %v, want %v",
						read.name, start, records, err, want)
				}
			}
		}
	}

	t.Run("foreign", func(t *testing.T) {
		other := NewCapture()
		if err := other.Append(testDataEvent(t, testBus0, 0x100, 0, []byte{9}, 0, DirectionReceive)); err != nil {
			t.Fatalf("append to the other capture: %v", err)
		}
		requireRejected(t, other.End(), ErrCursorStale)
	})

	t.Run("beyond the end", func(t *testing.T) {
		beyondRecord := end
		beyondRecord.record++
		requireRejected(t, beyondRecord, ErrCursorOutOfRange)
		beyondChunk := end
		beyondChunk.chunk++
		requireRejected(t, beyondChunk, ErrCursorOutOfRange)

		// A record past the end of a sealed chunk is out of range even though
		// later chunks hold that many records.
		beyondSealed := Cursor{
			generation: capture.generation,
			chunk:      0,
			record:     uint32(len(capture.chunks[0].records)),
		}
		requireRejected(t, beyondSealed, ErrCursorOutOfRange)
	})

	t.Run("reversed range", func(t *testing.T) {
		for _, read := range bounded {
			if records, err := read.read(end, early); !errors.Is(err, ErrCursorOutOfRange) || records != 0 {
				t.Errorf("%s over a reversed range = %d records, %v, want ErrCursorOutOfRange",
					read.name, records, err)
			}
		}
	})

	// Clear last: it invalidates every cursor minted above, which is exactly
	// the case a follower hits when the capture it tails is reset underneath it.
	t.Run("cleared", func(t *testing.T) {
		capture.Clear()
		if err := capture.Append(testDataEvent(t, testBus0, 0x100, 0, []byte{2}, 3, DirectionReceive)); err != nil {
			t.Fatalf("append after Clear: %v", err)
		}
		requireRejected(t, end, ErrCursorStale)

		// Oldest is the resynchronisation point: it reads what survives.
		frames, _, err := capture.FramesSince(capture.Oldest())
		if err != nil {
			t.Fatalf("FramesSince(Oldest()): %v", err)
		}
		if len(frames) != 1 {
			t.Fatalf("FramesSince(Oldest()) returned %d frames, want the one record after Clear", len(frames))
		}
	})
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
	// replaying the surviving records, and resynchronising from Oldest returns
	// everything the capture still holds.
	capture.Clear()
	revived := appendSeq(100, 0x100)
	stale, returned, err := capture.SeriesSince(keyA, cursorA)
	if !errors.Is(err, ErrCursorStale) {
		t.Fatalf("SeriesSince after Clear error = %v, want ErrCursorStale", err)
	}
	if len(stale) != 0 || returned != cursorA {
		t.Fatalf("stale SeriesSince returned %d frames and cursor %+v", len(stale), returned)
	}
	seriesA, _ = readSeries("resynchronised", capture.Oldest())
	requireEvents(t, "resynchronised after Clear", seriesA, []FrameEvent{revived})
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

// TestCaptureNextAcrossClear covers a Next that is already waiting when Clear
// discards the position it holds: it must report the loss promptly, and a
// caller reading from the beginning must keep waiting instead, because the zero
// Cursor still places.
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
		cursor Cursor
		err    error
	}
	stale := make(chan result, 1)
	go func() {
		_, next, err := capture.Next(ctx, key, held)
		stale <- result{next, err}
	}()
	waitForCaptureWaiter(t, capture, key)
	capture.Clear()

	select {
	case got := <-stale:
		if !errors.Is(got.err, ErrCursorStale) {
			t.Fatalf("waiting Next after Clear returned %v, want ErrCursorStale", got.err)
		}
		if got.cursor != held {
			t.Fatalf("waiting Next returned cursor %+v, want the cursor it was given", got.cursor)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting Next did not report Clear discarding its position")
	}
	capture.mu.RLock()
	leaked := len(capture.waiters)
	capture.mu.RUnlock()
	if leaked != 0 {
		t.Fatalf("%d waiter entries remain after a stale wake", leaked)
	}

	// A Next reading from the beginning walks the capture before it parks, so
	// its internal position goes stale while the caller's does not. It must
	// deliver the first frame of the new generation, not an error.
	if err := capture.Append(testDataEvent(t, testBus0, other.ID, 0, []byte{2}, 1, DirectionReceive)); err != nil {
		t.Fatalf("append unrelated frame: %v", err)
	}
	fromStart := make(chan result, 1)
	frames := make(chan FrameEvent, 1)
	go func() {
		event, next, err := capture.Next(ctx, key, Cursor{})
		frames <- event
		fromStart <- result{next, err}
	}()
	waitForCaptureWaiter(t, capture, key)
	capture.Clear()
	revived := testDataEvent(t, testBus0, key.ID, 0, []byte{3}, 2, DirectionReceive)
	if err := capture.Append(revived); err != nil {
		t.Fatalf("append after Clear: %v", err)
	}
	got := <-fromStart
	if got.err != nil {
		t.Fatalf("Next from the zero Cursor across Clear: %v", got.err)
	}
	if event := <-frames; !eventsEqual(event, revived) {
		t.Fatalf("Next from the zero Cursor returned %+v, want %+v", event, revived)
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
	churning.Add(2)
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
			if err != nil && !errors.Is(err, ErrCursorStale) {
				t.Errorf("EventsSince: %v", err)
				return
			}
			eventsCursor = next
			if err != nil {
				eventsCursor = capture.Oldest()
			}
			_, next, err = capture.BusEventsSince(testBus0, busEventsCursor)
			if err != nil && !errors.Is(err, ErrCursorStale) {
				t.Errorf("BusEventsSince: %v", err)
				return
			}
			busEventsCursor = next
			if err != nil {
				busEventsCursor = capture.Oldest()
			}
		}
	}()
	<-done
	churning.Wait()

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
