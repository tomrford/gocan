package gocan

import "errors"

// ErrInvalidCaptureRange reports a stale, foreign, out-of-bounds, or reversed
// cursor passed to a bounded Capture read. The zero Cursor is a valid boundary
// before the first retained record.
var ErrInvalidCaptureRange = errors.New("invalid capture range")

// WriteRecordsBetween sends every frame and event in the capture interval
// (start, end] to writer in Capture append order. The start cursor is excluded
// and the end cursor is included. A zero start begins at the first retained
// record. On success it returns end. Failure behavior matches
// WriteRecordsSince.
//
// Each non-zero cursor must be retained by this Capture. A stale, foreign,
// out-of-bounds, or reversed cursor returns ErrInvalidCaptureRange.
func (capture *Capture) WriteRecordsBetween(start, end Cursor, writer RecordWriter) (Cursor, error) {
	views, skip, firstChunk, err := capture.viewsBetween(start, end)
	if err != nil {
		return start, err
	}
	return writeRecords(views, skip, firstChunk, start, end, writer)
}

// FramesBetween returns owned copies of every frame in the capture interval
// (start, end], in append order. The start cursor is excluded and the end
// cursor is included. A zero start begins at the first retained record.
//
// Each non-zero cursor must be retained by this Capture. A stale, foreign,
// out-of-bounds, or reversed cursor returns ErrInvalidCaptureRange. Records
// appended after end are never included.
func (capture *Capture) FramesBetween(start, end Cursor) ([]FrameEvent, error) {
	views, skip, _, err := capture.viewsBetween(start, end)
	if err != nil {
		return nil, err
	}
	return framesFromViews(views, skip), nil
}

// EventsBetween returns every non-frame event in the capture interval
// (start, end], in append order. The start cursor is excluded and the end
// cursor is included. A zero start begins at the first retained record.
//
// Each non-zero cursor must be retained by this Capture. A stale, foreign,
// out-of-bounds, or reversed cursor returns ErrInvalidCaptureRange. Records
// appended after end are never included.
func (capture *Capture) EventsBetween(start, end Cursor) ([]Event, error) {
	views, skip, _, err := capture.viewsBetween(start, end)
	if err != nil {
		return nil, err
	}
	return eventsFromViews(views, skip), nil
}

// SeriesBetween returns owned copies of every frame matching key in the
// capture interval (start, end], in append order. The start cursor is excluded
// and the end cursor is included. A zero start begins at the first retained
// record.
//
// Each non-zero cursor must be retained by this Capture. A stale, foreign,
// out-of-bounds, or reversed cursor returns ErrInvalidCaptureRange. Records
// appended after end are never included.
func (capture *Capture) SeriesBetween(key FrameKey, start, end Cursor) ([]FrameEvent, error) {
	views, skip, _, err := capture.viewsBetween(start, end)
	if err != nil {
		return nil, err
	}

	var frames []FrameEvent
	for i := range views {
		from := 0
		if i == 0 {
			from = skip
		}
		for record := from; record < len(views[i].records); record++ {
			if views[i].records[record].kind != captureRecordFrame {
				continue
			}
			data := views[i].records[record].frameData()
			if views[i].keys[data.keyIndex] == key {
				frames = append(frames, views[i].frameAt(uint32(record)))
			}
		}
	}
	return frames, nil
}

// BusEventsBetween returns every event from bus in the capture interval
// (start, end], in append order. The start cursor is excluded and the end
// cursor is included. A zero start begins at the first retained record.
//
// Each non-zero cursor must be retained by this Capture. A stale, foreign,
// out-of-bounds, or reversed cursor returns ErrInvalidCaptureRange. Records
// appended after end are never included.
func (capture *Capture) BusEventsBetween(bus BusID, start, end Cursor) ([]Event, error) {
	views, skip, _, err := capture.viewsBetween(start, end)
	if err != nil {
		return nil, err
	}

	var events []Event
	for i := range views {
		from := 0
		if i == 0 {
			from = skip
		}
		for record := from; record < len(views[i].records); record++ {
			if views[i].records[record].kind == captureRecordEvent &&
				views[i].records[record].eventData().bus == bus {
				events = append(events, views[i].eventAt(uint32(record)))
			}
		}
	}
	return events, nil
}

// viewsBetween freezes the records in (start, end]. The returned views are
// clipped at end, so readers can use the same loops as an unbounded Since
// read. firstChunk is the capture index represented by views[0].
func (capture *Capture) viewsBetween(start, end Cursor) (views []captureView, skip int, firstChunk uint32, err error) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	generation := capture.generation
	capture.mu.RUnlock()

	viewAt := func(cursor Cursor) (captureView, bool) {
		if cursor == (Cursor{}) || cursor.generation != generation || int(cursor.chunk) >= len(chunks) {
			return captureView{}, cursor == (Cursor{})
		}
		if int(cursor.chunk) == len(chunks)-1 {
			return activeView, int(cursor.record) < len(activeView.records)
		}
		view := chunks[cursor.chunk].view()
		return view, int(cursor.record) < len(view.records)
	}

	if _, valid := viewAt(start); !valid {
		return nil, 0, 0, ErrInvalidCaptureRange
	}
	if _, valid := viewAt(end); !valid {
		return nil, 0, 0, ErrInvalidCaptureRange
	}
	if end == (Cursor{}) {
		if start != (Cursor{}) {
			return nil, 0, 0, ErrInvalidCaptureRange
		}
		return nil, 0, 0, nil
	}
	if start != (Cursor{}) &&
		(start.chunk > end.chunk || start.chunk == end.chunk && start.record > end.record) {
		return nil, 0, 0, ErrInvalidCaptureRange
	}
	if start == end {
		return nil, 0, 0, nil
	}

	first := 0
	if start != (Cursor{}) {
		first = int(start.chunk)
		skip = int(start.record) + 1
	}

	views = make([]captureView, int(end.chunk)-first+1)
	for i := range views {
		chunkIndex := first + i
		if chunkIndex == len(chunks)-1 {
			views[i] = activeView
		} else {
			views[i] = chunks[chunkIndex].view()
		}
	}
	views[len(views)-1].records = views[len(views)-1].records[:end.record+1]
	return views, skip, uint32(first), nil
}
