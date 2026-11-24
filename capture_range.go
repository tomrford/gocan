package gocan

import (
	"fmt"
	"iter"
)

// Range selects a half-open interval of records in a Capture.
//
// The zero Range selects every retained record through the capture's end at
// read time. Use Since for an open-ended interval starting after a cursor, or
// Between for a bounded interval (start, end].
type Range struct {
	start   Cursor
	end     Cursor
	bounded bool
}

// Since returns a Range selecting every record appended after cursor through
// the capture's end at read time. The zero Cursor selects all retained records.
func Since(cursor Cursor) Range {
	return Range{start: cursor}
}

// Between returns a Range selecting every record in the half-open interval
// (start, end]. The start cursor is excluded and the end cursor is included.
// The zero start begins at the first retained record, and the zero end denotes
// the boundary before all retained records (so Between(Cursor{}, Cursor{}) is
// empty and a non-zero start with a zero end is reversed unless start is the
// current prune boundary). Records appended after end are never included.
func Between(start, end Cursor) Range {
	return Range{start: start, end: end, bounded: true}
}

type captureWindow struct {
	sealed        []*captureChunk
	singleSealed  *captureChunk
	activeView    captureView
	startBoundary int
	endBoundary   int
	hasActive     bool
}

func (window captureWindow) sealedCount() int {
	if len(window.sealed) > 0 {
		return len(window.sealed)
	}
	if window.singleSealed != nil {
		return 1
	}
	return 0
}

func (window captureWindow) chunkCount() int {
	count := window.sealedCount()
	if window.hasActive {
		count++
	}
	return count
}

func (window captureWindow) sealedAt(index int) *captureChunk {
	if len(window.sealed) > 0 {
		return window.sealed[index]
	}
	return window.singleSealed
}

func (window captureWindow) viewAt(index int) captureView {
	if window.hasActive && index == window.chunkCount()-1 {
		return window.activeView
	}
	return window.sealedAt(index).view()
}

func (window captureWindow) boundsAt(index, recordCount int) (from, to int) {
	if index == 0 {
		from = window.startBoundary + 1
	}
	to = recordCount - 1
	if index == window.chunkCount()-1 {
		to = window.endBoundary
	}
	return from, to
}

func makeWindow(
	chunks []*captureChunk,
	first, last int,
	activeView captureView,
	startBoundary, endBoundary int,
) captureWindow {
	hasActive := last == len(chunks)-1
	sealedEnd := last + 1
	if hasActive {
		sealedEnd = last
	}
	// If the start cursor is the last record of the first sealed chunk and
	// later chunks follow, drop that exhausted chunk so the iterator does not
	// retain it.
	if first < last && startBoundary == len(chunks[first].records)-1 {
		first++
		startBoundary = -1
	}

	window := captureWindow{
		startBoundary: startBoundary,
		endBoundary:   endBoundary,
		hasActive:     hasActive,
	}
	if hasActive {
		window.activeView = activeView
	}
	switch sealedCount := sealedEnd - first; {
	case sealedCount == 1:
		window.singleSealed = chunks[first]
	case sealedCount > 1:
		// Copy into a fresh slice so the iterator does not pin capture.chunks's
		// backing array (and unrelated pruned or later-rotated chunks).
		window.sealed = append([]*captureChunk(nil), chunks[first:sealedEnd]...)
	}
	return window
}

func (capture *Capture) window(r Range) (window captureWindow, generation uint64, next Cursor, empty bool, err error) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	generation = capture.generation
	pruneSeam := capture.pruneSeam
	capture.mu.RUnlock()

	window, next, empty, err = resolveWindow(r, generation, chunks, activeView, pruneSeam)
	return window, generation, next, empty, err
}

func resolveWindow(
	r Range,
	generation uint64,
	chunks []*captureChunk,
	activeView captureView,
	pruneSeam Cursor,
) (window captureWindow, next Cursor, empty bool, err error) {
	if r.bounded {
		first, startBoundary, last, endBoundary, isEmpty, err := locateRange(
			r.start, r.end, generation, chunks, len(activeView.records), pruneSeam,
		)
		if err != nil {
			return captureWindow{}, Cursor{}, false, err
		}
		if isEmpty {
			return captureWindow{}, r.end, true, nil
		}
		return makeWindow(chunks, first, last, activeView, startBoundary, endBoundary), r.end, false, nil
	}

	first, startBoundary, err := locateCursor(r.start, generation, chunks, len(activeView.records), pruneSeam)
	if err != nil {
		return captureWindow{}, Cursor{}, false, err
	}
	last := len(chunks) - 1
	endBoundary := len(activeView.records) - 1

	next = r.start
	if count := len(activeView.records); count > 0 {
		next = Cursor{generation: generation, chunk: activeView.sequence, record: uint32(count - 1)}
	}
	if first == last && startBoundary == endBoundary {
		return captureWindow{}, next, true, nil
	}
	return makeWindow(chunks, first, last, activeView, startBoundary, endBoundary), next, false, nil
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

func emptySeq[T any](func(T) bool) {}

// Frames returns an iterator over owned copies of every frame in r, in append
// order, together with the cursor of the interval's upper bound.
//
// For the zero Range or Since, the returned cursor is the newest record
// observed anywhere in the capture at call time (or the starting cursor when no
// newer records exist). For Between, it is the interval's end cursor. Because
// the returned cursor marks the end of the entire snapshot, stopping iteration
// early and later resuming with Since(next) skips any unread records in that
// snapshot.
//
// The snapshot is frozen before Frames returns, so later appends are not
// included and iterating holds no lock: callbacks may safely call back into
// Capture or stop early, and the returned iterator may be traversed more than
// once. Retaining the iterator keeps the chunks in its observed window
// reachable until the iterator is discarded, even after Prune or Clear.
//
// If a cursor in r cannot be placed or the range is reversed, Frames returns
// an empty iterator, the range's start cursor, and an error wrapping
// ErrCursorOutOfRange.
func (capture *Capture) Frames(r Range) (iter.Seq[FrameEvent], Cursor, error) {
	window, _, next, empty, err := capture.window(r)
	if err != nil {
		return emptySeq[FrameEvent], r.start, err
	}
	if empty {
		return emptySeq[FrameEvent], next, nil
	}
	return window.iterFrames, next, nil
}

// Events returns an iterator over every non-frame event in r, in append order,
// together with the cursor of the interval's upper bound.
//
// Snapshot, locking, retention, cursor advancement, and error semantics match
// Frames.
func (capture *Capture) Events(r Range) (iter.Seq[Event], Cursor, error) {
	window, _, next, empty, err := capture.window(r)
	if err != nil {
		return emptySeq[Event], r.start, err
	}
	if empty {
		return emptySeq[Event], next, nil
	}
	return window.iterEvents, next, nil
}

func (window captureWindow) iterFrames(yield func(FrameEvent) bool) {
	count := window.chunkCount()
	for i := range count {
		view := window.viewAt(i)
		from, to := window.boundsAt(i, len(view.records))
		for record := from; record <= to; record++ {
			if view.records[record].kind == captureRecordFrame {
				if !yield(view.frameAt(uint32(record))) {
					return
				}
			}
		}
	}
}

func (window captureWindow) iterEvents(yield func(Event) bool) {
	count := window.chunkCount()
	for i := range count {
		view := window.viewAt(i)
		from, to := window.boundsAt(i, len(view.records))
		for record := from; record <= to; record++ {
			if view.records[record].kind == captureRecordEvent {
				if !yield(view.eventAt(uint32(record))) {
					return
				}
			}
		}
	}
}

// Series returns an iterator over owned copies of every frame matching key in
// r, in append order, together with the cursor of the interval's upper bound.
//
// Snapshot, locking, retention, cursor advancement, and error semantics match
// Frames. The returned cursor tracks global capture position rather than the
// last matching frame, so advancing from it skips non-matching traffic on later
// reads.
func (capture *Capture) Series(key FrameKey, r Range) (iter.Seq[FrameEvent], Cursor, error) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	activeState := capture.active.keyStates[key]
	generation := capture.generation
	pruneSeam := capture.pruneSeam
	capture.mu.RUnlock()

	window, next, empty, err := resolveWindow(r, generation, chunks, activeView, pruneSeam)
	if err != nil {
		return emptySeq[FrameEvent], r.start, err
	}
	if empty {
		return emptySeq[FrameEvent], next, nil
	}
	if !window.hasActive {
		activeState = captureKeyState{}
	}
	return seriesIter{window: window, key: key, activeState: activeState}.yield, next, nil
}

type seriesIter struct {
	window      captureWindow
	key         FrameKey
	activeState captureKeyState
}

func matchFrameKeyIndex(record captureRecord, keyIndex uint32) bool {
	return record.kind == captureRecordFrame && record.frameData().keyIndex == keyIndex
}

func matchEventBus(record captureRecord, bus uint32) bool {
	return record.kind == captureRecordEvent && uint32(record.eventData().bus) == bus
}

func (it seriesIter) yield(yield func(FrameEvent) bool) {
	var heapIdx []uint32
	count := it.window.chunkCount()
	for i := range count {
		var view captureView
		var state captureKeyState
		if it.window.hasActive && i == count-1 {
			view, state = it.window.activeView, it.activeState
		} else {
			chunk := it.window.sealedAt(i)
			view, state = chunk.view(), chunk.keyStates[it.key]
		}
		from, to := it.window.boundsAt(i, len(view.records))
		var ok bool
		heapIdx, ok = yieldIndexedChunk(
			view,
			state.captureIndexState,
			from,
			to,
			state.index,
			matchFrameKeyIndex,
			captureView.frameAt,
			heapIdx,
			yield,
		)
		if !ok {
			return
		}
	}
}

// BusEvents returns an iterator over every non-frame event from bus in r, in
// append order, together with the cursor of the interval's upper bound.
//
// Snapshot, locking, retention, cursor advancement, and error semantics match
// Frames. The returned cursor tracks global capture position rather than the
// last matching event, so advancing from it skips unrelated traffic on later
// reads.
func (capture *Capture) BusEvents(bus BusID, r Range) (iter.Seq[Event], Cursor, error) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	activeState := capture.active.eventStates[bus]
	generation := capture.generation
	pruneSeam := capture.pruneSeam
	capture.mu.RUnlock()

	window, next, empty, err := resolveWindow(r, generation, chunks, activeView, pruneSeam)
	if err != nil {
		return emptySeq[Event], r.start, err
	}
	if empty {
		return emptySeq[Event], next, nil
	}
	if !window.hasActive {
		activeState = captureIndexState{}
	}
	return busEventsIter{window: window, bus: bus, activeState: activeState}.yield, next, nil
}

type busEventsIter struct {
	window      captureWindow
	bus         BusID
	activeState captureIndexState
}

func (it busEventsIter) yield(yield func(Event) bool) {
	var heapIdx []uint32
	bus := it.bus
	count := it.window.chunkCount()
	for i := range count {
		var view captureView
		var state captureIndexState
		if it.window.hasActive && i == count-1 {
			view, state = it.window.activeView, it.activeState
		} else {
			chunk := it.window.sealedAt(i)
			view, state = chunk.view(), chunk.eventStates[bus]
		}
		from, to := it.window.boundsAt(i, len(view.records))
		var ok bool
		heapIdx, ok = yieldIndexedChunk(
			view,
			state,
			from,
			to,
			uint32(bus),
			matchEventBus,
			captureView.eventAt,
			heapIdx,
			yield,
		)
		if !ok {
			return
		}
	}
}

func yieldIndexedChunk[T any](
	view captureView,
	state captureIndexState,
	from int,
	to int,
	matchTag uint32,
	match func(captureRecord, uint32) bool,
	decode func(captureView, uint32) T,
	heapIdx []uint32,
	yield func(T) bool,
) ([]uint32, bool) {
	if state.count == 0 || from > to {
		return heapIdx, true
	}
	if int(state.lastRecord) < from {
		return heapIdx, true
	}
	span := to - from + 1
	// Forward scan directly when the chunk is dense relative to the requested
	// span (including narrow historical slices with many newer matches): zero
	// scratch allocation, sequential memory access, and immediate early stop.
	if int64(state.count)*4 >= int64(span) {
		for record := from; record <= to; record++ {
			if match(view.records[record], matchTag) {
				if !yield(decode(view, uint32(record))) {
					return heapIdx, false
				}
			}
		}
		return heapIdx, true
	}

	// Sparse path: state.count*4 < span, so skipping hits past `to` visits
	// strictly fewer than span/4 records.
	head := state.lastRecord
	for head != noCaptureRecord && int(head) > to {
		head = view.records[head].previous
	}
	if head == noCaptureRecord || int(head) < from {
		return heapIdx, true
	}

	// Collect matching record indices in reverse into a stack buffer, spilling
	// to a reusable heap buffer only when a single chunk has >64 matches.
	var idxBuf [64]uint32
	n := 0
	record := head
	for record != noCaptureRecord && int(record) >= from && n < len(idxBuf) {
		idxBuf[n] = record
		n++
		record = view.records[record].previous
	}
	if record == noCaptureRecord || int(record) < from {
		for i := n - 1; i >= 0; i-- {
			if !yield(decode(view, idxBuf[i])) {
				return heapIdx, false
			}
		}
		return heapIdx, true
	}

	need := max(len(idxBuf)*2, int(state.count))
	if cap(heapIdx) < need {
		heapIdx = make([]uint32, 0, need)
	}
	indices := append(heapIdx[:0], idxBuf[:]...)
	for ; record != noCaptureRecord && int(record) >= from; record = view.records[record].previous {
		indices = append(indices, record)
	}
	heapIdx = indices[:0]
	for i := len(indices) - 1; i >= 0; i-- {
		if !yield(decode(view, indices[i])) {
			return heapIdx, false
		}
	}
	return heapIdx, true
}

// WriteRecords sends every frame and event in r to writer in Capture append
// order.
//
// On success it returns the cursor of the interval's upper bound (the newest
// record observed for the zero Range or Since, or end for Between). On a cursor
// error it writes nothing and returns the range's start cursor with an error
// wrapping ErrCursorOutOfRange. On a writer failure it returns the cursor of
// the last record whose writer call succeeded and a *RecordWriteError whose
// Cursor identifies the failed record. Retry from the returned cursor, or
// deliberately skip the failed record by continuing from the error's Cursor.
//
// A writer may commit partial output before returning an error. Capture tracks
// completed writer calls, not the transactional state of the underlying output.
func (capture *Capture) WriteRecords(r Range, writer RecordWriter) (Cursor, error) {
	window, generation, next, empty, err := capture.window(r)
	if err != nil {
		return r.start, err
	}
	if empty {
		return next, nil
	}
	return writeRecords(window, generation, r.start, next, writer)
}

func writeRecords(window captureWindow, generation uint64, lastWritten, success Cursor, writer RecordWriter) (Cursor, error) {
	count := window.chunkCount()
	for i := range count {
		view := window.viewAt(i)
		from, to := window.boundsAt(i, len(view.records))
		for record := from; record <= to; record++ {
			var err error
			switch view.records[record].kind {
			case captureRecordFrame:
				err = writer.WriteFrame(view.frameAt(uint32(record)))
			case captureRecordEvent:
				err = writer.WriteEvent(view.eventAt(uint32(record)))
			default:
				panic("gocan: capture record has an unknown kind")
			}
			recordCursor := Cursor{
				generation: generation,
				chunk:      view.sequence,
				record:     uint32(record),
			}
			if err != nil {
				return lastWritten, &RecordWriteError{Cursor: recordCursor, Err: err}
			}
			lastWritten = recordCursor
		}
	}
	return success, nil
}
