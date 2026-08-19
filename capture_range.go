package gocan

import "fmt"

// WriteRecordsBetween sends every frame and event in the capture interval
// (start, end] to writer in Capture append order. The start cursor is excluded
// and the end cursor is included. A zero start begins at the first retained
// record. On success it returns end. Failure behavior matches
// WriteRecordsSince.
func (capture *Capture) WriteRecordsBetween(start, end Cursor, writer RecordWriter) (Cursor, error) {
	views, skip, err := capture.viewsBetween(start, end)
	if err != nil {
		return start, err
	}
	return writeRecords(views, skip, start, end, writer)
}

// FramesBetween returns owned copies of every frame in the capture interval
// (start, end], in append order. The start cursor is excluded and the end
// cursor is included. A zero start begins at the first retained record. Records
// appended after end are never included.
func (capture *Capture) FramesBetween(start, end Cursor) ([]FrameEvent, error) {
	views, skip, err := capture.viewsBetween(start, end)
	if err != nil {
		return nil, err
	}
	return framesFromViews(views, skip), nil
}

// EventsBetween returns every non-frame event in the capture interval
// (start, end], in append order. The start cursor is excluded and the end
// cursor is included. A zero start begins at the first retained record. Records
// appended after end are never included.
func (capture *Capture) EventsBetween(start, end Cursor) ([]Event, error) {
	views, skip, err := capture.viewsBetween(start, end)
	if err != nil {
		return nil, err
	}
	return eventsFromViews(views, skip), nil
}

// SeriesBetween returns owned copies of every frame matching key in the
// capture interval (start, end], in append order. The start cursor is excluded
// and the end cursor is included. A zero start begins at the first retained
// record. Records appended after end are never included.
func (capture *Capture) SeriesBetween(key FrameKey, start, end Cursor) ([]FrameEvent, error) {
	views, skip, err := capture.viewsBetween(start, end)
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
// cursor is included. A zero start begins at the first retained record. Records
// appended after end are never included.
func (capture *Capture) BusEventsBetween(bus BusID, start, end Cursor) ([]Event, error) {
	views, skip, err := capture.viewsBetween(start, end)
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
// read.
func (capture *Capture) viewsBetween(start, end Cursor) (views []captureView, skip int, err error) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	generation := capture.generation
	capture.mu.RUnlock()

	// The end boundary is the last record the interval includes, so a zero end
	// bounds an empty interval at the start of the capture.
	last, endRecord, err := locateCursor(end, generation, chunks)
	if err != nil {
		return nil, 0, fmt.Errorf("capture range end: %w", err)
	}
	first, startRecord, err := locateCursor(start, generation, chunks)
	if err != nil {
		return nil, 0, fmt.Errorf("capture range start: %w", err)
	}
	if first > last || first == last && startRecord > endRecord {
		return nil, 0, fmt.Errorf("capture range end precedes its start: %w", ErrCursorOutOfRange)
	}

	views = make([]captureView, last-first+1)
	for i := range views {
		chunkIndex := first + i
		if chunkIndex == len(chunks)-1 {
			views[i] = activeView
		} else {
			views[i] = chunks[chunkIndex].view()
		}
	}
	// The end record is cut from the frozen tail view, so it has to lie inside
	// it. Only a library bug can name a record the chunk does not hold.
	tail := views[len(views)-1].records
	if endRecord+1 > len(tail) {
		return nil, 0, fmt.Errorf("capture range end: %w", ErrCursorOutOfRange)
	}
	views[len(views)-1].records = tail[:endRecord+1]
	return views, startRecord + 1, nil
}
