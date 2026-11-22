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
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	activeState := capture.active.keyStates[key].captureIndexState
	generation := capture.generation
	pruneSeam := capture.pruneSeam
	capture.mu.RUnlock()

	return collectBetween(
		start,
		end,
		generation,
		chunks,
		activeView,
		activeState,
		pruneSeam,
		func(chunk *captureChunk) captureIndexState { return chunk.keyStates[key].captureIndexState },
		func(view captureView, record uint32) bool {
			return view.records[record].kind == captureRecordFrame &&
				view.keys[view.records[record].frameData().keyIndex] == key
		},
		captureView.frameAt,
	)
}

// BusEventsBetween returns every event from bus in the capture interval
// (start, end], in append order. The start cursor is excluded and the end
// cursor is included. A zero start begins at the first retained record. Records
// appended after end are never included.
func (capture *Capture) BusEventsBetween(bus BusID, start, end Cursor) ([]Event, error) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	activeState := capture.active.eventStates[bus]
	generation := capture.generation
	pruneSeam := capture.pruneSeam
	capture.mu.RUnlock()

	return collectBetween(
		start,
		end,
		generation,
		chunks,
		activeView,
		activeState,
		pruneSeam,
		func(chunk *captureChunk) captureIndexState { return chunk.eventStates[bus] },
		func(view captureView, record uint32) bool {
			return view.records[record].kind == captureRecordEvent &&
				view.records[record].eventData().bus == bus
		},
		captureView.eventAt,
	)
}

func collectBetween[T any](
	start, end Cursor,
	generation uint64,
	chunks []*captureChunk,
	activeView captureView,
	activeState captureIndexState,
	pruneSeam Cursor,
	chunkState func(*captureChunk) captureIndexState,
	match func(captureView, uint32) bool,
	decode func(captureView, uint32) T,
) ([]T, error) {
	first, startRecord, last, endRecord, empty, err := locateRange(
		start, end, generation, chunks, len(activeView.records), pruneSeam,
	)
	if err != nil || empty {
		return nil, err
	}

	var viewBuf [4]captureView
	var stateBuf [4]captureIndexState
	count := last - first + 1
	var views []captureView
	var states []captureIndexState
	if count <= len(viewBuf) {
		views = viewBuf[:count]
		states = stateBuf[:count]
	} else {
		views = make([]captureView, count)
		states = make([]captureIndexState, count)
	}
	for i := range views {
		chunkIndex := first + i
		if chunkIndex == len(chunks)-1 {
			views[i], states[i] = activeView, activeState
		} else {
			views[i] = chunks[chunkIndex].view()
			states[i] = chunkState(chunks[chunkIndex])
		}
	}
	return collectIndexedRecords(views, states, startRecord, endRecord, match, decode), nil
}

func locateRange(
	start, end Cursor,
	generation uint64,
	chunks []*captureChunk,
	activeRecords int,
	pruneSeam Cursor,
) (first, startRecord, last, endRecord int, empty bool, err error) {
	last, endRecord, err = locateCursor(end, generation, chunks, activeRecords, pruneSeam)
	if err != nil {
		return 0, 0, 0, 0, false, fmt.Errorf("gocan: capture range end: %w", err)
	}
	first, startRecord, err = locateCursor(start, generation, chunks, activeRecords, pruneSeam)
	if err != nil {
		return 0, 0, 0, 0, false, fmt.Errorf("gocan: capture range start: %w", err)
	}
	if first > last || first == last && startRecord > endRecord {
		return 0, 0, 0, 0, false, fmt.Errorf("gocan: capture range end precedes its start: %w", ErrCursorOutOfRange)
	}
	if first == last && startRecord == endRecord {
		return 0, 0, 0, 0, true, nil
	}
	return first, startRecord, last, endRecord, false, nil
}

func (capture *Capture) viewsBetween(start, end Cursor) (views []captureView, skip int, err error) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	generation := capture.generation
	pruneSeam := capture.pruneSeam
	capture.mu.RUnlock()

	first, startRecord, last, endRecord, empty, err := locateRange(
		start, end, generation, chunks, len(activeView.records), pruneSeam,
	)
	if err != nil || empty {
		return nil, 0, err
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
	views[len(views)-1].records = views[len(views)-1].records[:endRecord+1]
	return views, startRecord + 1, nil
}
