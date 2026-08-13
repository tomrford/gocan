package gocan

// WriteRecordsBetween sends every frame and event in the capture interval
// (start, end] to writer in Capture append order. The start cursor is excluded
// and the end cursor is included. A zero start begins at the first retained
// record. On success it returns end. Failure behavior matches
// WriteRecordsSince.
//
// An invalid start behaves like the zero Cursor. An invalid, foreign,
// out-of-bounds, or reversed end writes no records and returns start.
func (capture *Capture) WriteRecordsBetween(start, end Cursor, writer RecordWriter) (Cursor, error) {
	views, skip, firstChunk, ok := capture.viewsBetween(start, end)
	if !ok {
		return start, nil
	}
	return writeRecords(views, skip, firstChunk, start, end, writer)
}

// FramesBetween returns owned copies of every frame in the capture interval
// (start, end], in append order. The start cursor is excluded and the end
// cursor is included. A zero start begins at the first retained record.
//
// An invalid start behaves like the zero Cursor. An invalid, foreign,
// out-of-bounds, or reversed end returns no records. Records appended after end
// are never included.
func (capture *Capture) FramesBetween(start, end Cursor) []FrameEvent {
	views, skip, _, ok := capture.viewsBetween(start, end)
	if !ok {
		return nil
	}
	return framesFromViews(views, skip)
}

// EventsBetween returns every non-frame event in the capture interval
// (start, end], in append order. The start cursor is excluded and the end
// cursor is included. A zero start begins at the first retained record.
//
// An invalid start behaves like the zero Cursor. An invalid, foreign,
// out-of-bounds, or reversed end returns no records. Records appended after end
// are never included.
func (capture *Capture) EventsBetween(start, end Cursor) []Event {
	views, skip, _, ok := capture.viewsBetween(start, end)
	if !ok {
		return nil
	}
	return eventsFromViews(views, skip)
}

// SeriesBetween returns owned copies of every frame matching key in the
// capture interval (start, end], in append order. The start cursor is excluded
// and the end cursor is included. A zero start begins at the first retained
// record.
//
// An invalid start behaves like the zero Cursor. An invalid, foreign,
// out-of-bounds, or reversed end returns no records. Records appended after end
// are never included.
func (capture *Capture) SeriesBetween(key FrameKey, start, end Cursor) []FrameEvent {
	views, skip, _, ok := capture.viewsBetween(start, end)
	if !ok {
		return nil
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
	return frames
}

// BusEventsBetween returns every event from bus in the capture interval
// (start, end], in append order. The start cursor is excluded and the end
// cursor is included. A zero start begins at the first retained record.
//
// An invalid start behaves like the zero Cursor. An invalid, foreign,
// out-of-bounds, or reversed end returns no records. Records appended after end
// are never included.
func (capture *Capture) BusEventsBetween(bus BusID, start, end Cursor) []Event {
	views, skip, _, ok := capture.viewsBetween(start, end)
	if !ok {
		return nil
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
	return events
}

// viewsBetween freezes the records in (start, end]. The returned views are
// clipped at end, so readers can use the same loops as an unbounded Since
// read. firstChunk is the capture index represented by views[0].
func (capture *Capture) viewsBetween(start, end Cursor) (views []captureView, skip int, firstChunk uint32, ok bool) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	generation := capture.generation
	capture.mu.RUnlock()

	if end.generation != generation || int(end.chunk) >= len(chunks) {
		return nil, 0, 0, false
	}

	var endView captureView
	if int(end.chunk) == len(chunks)-1 {
		endView = activeView
	} else {
		endView = chunks[end.chunk].view()
	}
	if int(end.record) >= len(endView.records) {
		return nil, 0, 0, false
	}

	first := 0
	if start.generation == generation && int(start.chunk) < len(chunks) {
		if start.chunk > end.chunk || start.chunk == end.chunk && start.record >= end.record {
			return nil, 0, 0, false
		}
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
	return views, skip, uint32(first), true
}
