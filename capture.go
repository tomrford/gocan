package gocan

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// These capacities fill together for 64-byte CAN FD frames. Classical CAN
	// reaches the record limit first. Replacement chunks preserve the fill
	// ratio of the chunk that caused rotation; see replacementCapacities.
	initialCaptureChunkPayloadCapacity = 8 << 20
	initialCaptureChunkRecordCapacity  = initialCaptureChunkPayloadCapacity / MaxDataLength
	minimumCaptureChunkPayloadCapacity = initialCaptureChunkPayloadCapacity / 16
	minimumCaptureChunkRecordCapacity  = initialCaptureChunkRecordCapacity / 16

	noCaptureRecord = ^uint32(0)
)

// Capture stores explicitly managed, multi-bus frames and events in append
// order.
//
// The zero Capture is not usable; create captures with NewCapture.
//
// Capture is safe for concurrent append and read operations. Frame payloads
// returned to callers own their payload storage. Each read observes the
// capture as it was at call time; later appends are not included. A read that
// starts before Clear continues against the records it observed.
//
// Capture is a low-level building block with no retention policy: it retains
// every record until Clear is called and memory grows without bound. Layers
// that need bounded retention, rotation, or persistence must provide it on
// top of Capture.
type Capture struct {
	mu sync.RWMutex

	generation uint64
	chunks     []*captureChunk
	active     *captureChunk
	next       *captureChunk

	allocatingNext bool
	latest         map[FrameKey]captureLocation
	waiters        map[FrameKey]*captureWaiter
	length         int
}

// captureEpochs issues process-unique generation numbers, so a cursor can
// never match a capture other than the one, in the generation, that minted it.
var captureEpochs atomic.Uint64

// NewCapture creates an empty capture.
func NewCapture() *Capture {
	active := newCaptureChunk(
		initialCaptureChunkRecordCapacity,
		initialCaptureChunkPayloadCapacity,
	)
	return &Capture{
		generation: captureEpochs.Add(1),
		chunks:     []*captureChunk{active},
		active:     active,
		next: newCaptureChunk(
			initialCaptureChunkRecordCapacity,
			initialCaptureChunkPayloadCapacity,
		),
		latest: make(map[FrameKey]captureLocation),
	}
}

type captureRecordKind uint8

const (
	captureRecordFrame captureRecordKind = iota + 1
	captureRecordEvent
)

// captureRecord is the common 24-byte storage envelope for frames and events.
// Variant-specific fields are encoded and decoded only through typed helpers.
// Records are written exactly once before append releases the capture mutex
// and are never modified afterwards.
type captureRecord struct {
	timestamp int64
	word0     uint32
	word1     uint32
	previous  uint32
	kind      captureRecordKind
	detail0   uint8
	detail1   uint8
	detail2   uint8
}

// frameRecordData is the typed frame view of a capture record. Direction is
// retained once in the key table rather than in every record. Previous links
// to the prior frame with the same key in the same chunk, or noCaptureRecord.
type frameRecordData struct {
	payloadOffset uint32
	keyIndex      uint32
	previous      uint32
	dlc           uint8
	flags         FrameFlags
}

// eventRecordData is the typed event view of a capture record. Previous links
// to the prior event from the same bus in the same chunk, or noCaptureRecord.
// Word 0 and the upper seven bits of detail 2 remain free so a future native
// diagnostic payload can use the chunk payload buffer through an offset and
// length.
type eventRecordData struct {
	bus              BusID
	previous         uint32
	kind             EventKind
	state            ControllerState
	txErrorCount     uint8
	rxErrorCount     uint8
	errorCountsKnown bool
}

func makeFrameRecord(timestamp int64, data frameRecordData) captureRecord {
	return captureRecord{
		timestamp: timestamp,
		word0:     data.payloadOffset,
		word1:     data.keyIndex,
		previous:  data.previous,
		kind:      captureRecordFrame,
		detail0:   data.dlc,
		detail1:   uint8(data.flags),
	}
}

func (record captureRecord) frameData() frameRecordData {
	if record.kind != captureRecordFrame {
		panic("gocan: capture record is not a frame")
	}
	return frameRecordData{
		payloadOffset: record.word0,
		keyIndex:      record.word1,
		previous:      record.previous,
		dlc:           record.detail0,
		flags:         FrameFlags(record.detail1),
	}
}

func makeEventRecord(timestamp int64, data eventRecordData) captureRecord {
	var detail2 uint8
	if data.errorCountsKnown {
		detail2 = 1
	}
	return captureRecord{
		timestamp: timestamp,
		word1: uint32(data.bus) |
			uint32(data.kind)<<16 |
			uint32(data.state)<<24,
		previous: data.previous,
		kind:     captureRecordEvent,
		detail0:  data.txErrorCount,
		detail1:  data.rxErrorCount,
		detail2:  detail2,
	}
}

func (record captureRecord) eventData() eventRecordData {
	if record.kind != captureRecordEvent {
		panic("gocan: capture record is not an event")
	}
	return eventRecordData{
		bus:              BusID(uint16(record.word1)),
		previous:         record.previous,
		kind:             EventKind(uint8(record.word1 >> 16)),
		state:            ControllerState(uint8(record.word1 >> 24)),
		txErrorCount:     record.detail0,
		rxErrorCount:     record.detail1,
		errorCountsKnown: record.detail2&1 != 0,
	}
}

type captureChunk struct {
	keys        []FrameKey
	keyStates   map[FrameKey]captureKeyState
	eventStates map[BusID]captureEventState
	records     []captureRecord
	payload     []byte
}

type captureKeyState struct {
	index      uint32
	lastRecord uint32
	count      uint32
}

type captureEventState struct {
	lastRecord uint32
	count      uint32
}

type captureLocation struct {
	chunk  *captureChunk
	record uint32
}

// captureWaiter parks the Next calls following one key. The append that wakes
// them carries its own record, so a woken Next needs no second walk. A wake
// with a zero cursor carries no record and means Clear discarded the position
// the waiters hold.
type captureWaiter struct {
	ready  chan struct{}
	count  int
	event  FrameEvent
	cursor Cursor
}

func newCaptureChunk(recordCapacity, payloadCapacity int) *captureChunk {
	return &captureChunk{
		keyStates:   make(map[FrameKey]captureKeyState),
		eventStates: make(map[BusID]captureEventState),
		records:     make([]captureRecord, 0, recordCapacity),
		payload:     make([]byte, 0, payloadCapacity),
	}
}

// ErrCursorOutOfRange indicates a cursor the capture cannot place: it belongs
// to another Capture, names records discarded by Clear or pruning, lies beyond
// the capture's end, or follows the end of a requested range.
var ErrCursorOutOfRange = errors.New("capture cursor is out of range")

// Cursor identifies a position in a capture's append order: the boundary just
// after one stored record. The zero Cursor is the position before every
// record, so every read accepts it.
//
// Cursors track global capture position, not per-series progress, so a cursor
// returned by one Since read may be passed to any Since read of the same
// Capture and never moves backwards. Cursors hold no references, so retaining
// one costs nothing.
//
// A cursor the capture cannot place fails the read that carries it with
// ErrCursorOutOfRange, as does a range whose end precedes its start. A failed
// read returns no records and the cursor it was given, so a caller either
// reports the loss or resynchronises from the zero Cursor or End. No read
// silently re-reads or skips history. A range whose end equals its start is
// empty, not an error.
type Cursor struct {
	generation uint64
	chunk      uint32
	record     uint32
}

// locateCursor places cursor in a snapshot of chunks taken in generation. It
// reports the chunk a read starts in and the record boundary within it: that
// record and every earlier one lie before the cursor.
//
// Generations are process-unique and chunks are never removed, so a cursor
// that matches the generation names a chunk that still exists and a record
// that chunk still holds. The bound is therefore a guard against a library
// bug, not a caller mistake.
func locateCursor(cursor Cursor, generation uint64, chunks []*captureChunk) (chunk, boundary int, err error) {
	if cursor == (Cursor{}) {
		return 0, -1, nil
	}
	if cursor.generation != generation {
		return 0, 0, ErrCursorOutOfRange
	}
	if int(cursor.chunk) >= len(chunks) {
		return 0, 0, ErrCursorOutOfRange
	}
	return int(cursor.chunk), int(cursor.record), nil
}

func (chunk *captureChunk) appendPayload(data []byte) uint32 {
	offset := uint32(len(chunk.payload))
	chunk.payload = append(chunk.payload, data...)
	return offset
}

// captureView freezes one chunk's slice headers. A view of a sealed chunk may
// be taken at any time; a view of the active chunk must be taken while
// holding the capture mutex. Either way the storage a view references is
// write-once, so the view may be read freely after the mutex is released.
type captureView struct {
	records []captureRecord
	keys    []FrameKey
	payload []byte
}

// RecordWriter consumes frames and events in Capture append order. Format
// packages implement this interface and decide how much detail their file
// format can preserve.
type RecordWriter interface {
	WriteFrame(FrameEvent) error
	WriteEvent(Event) error
}

// RecordWriteError reports a RecordWriter failure. Cursor identifies the
// record whose writer call failed. The cursor returned separately by
// WriteRecordsSince identifies the last record whose writer call succeeded.
type RecordWriteError struct {
	Cursor Cursor
	Err    error
}

func (err *RecordWriteError) Error() string {
	return fmt.Sprintf("write capture record: %v", err.Err)
}

func (err *RecordWriteError) Unwrap() error {
	return err.Err
}

func (chunk *captureChunk) view() captureView {
	return captureView{
		records: chunk.records,
		keys:    chunk.keys,
		payload: chunk.payload,
	}
}

// frameAt reconstructs the record at index as an owned FrameEvent. The
// timestamp is rebuilt in UTC from stored wall-clock nanoseconds; see Append.
func (view captureView) frameAt(index uint32) FrameEvent {
	record := view.records[index]
	data := record.frameData()
	key := view.keys[data.keyIndex]
	frame := Frame{
		ID:    key.ID,
		DLC:   data.dlc,
		Flags: data.flags,
	}
	payloadLength := uint32(frame.DataLength())
	copy(frame.Data[:], view.payload[data.payloadOffset:data.payloadOffset+payloadLength])
	return FrameEvent{
		Bus:       key.Bus,
		Timestamp: time.Unix(0, record.timestamp).UTC(),
		Direction: key.Direction,
		Frame:     frame,
	}
}

func (view captureView) eventAt(index uint32) Event {
	record := view.records[index]
	data := record.eventData()
	return Event{
		Bus:              data.bus,
		Timestamp:        time.Unix(0, record.timestamp).UTC(),
		Kind:             data.kind,
		ControllerState:  data.state,
		TXErrorCount:     data.txErrorCount,
		RXErrorCount:     data.rxErrorCount,
		ErrorCountsKnown: data.errorCountsKnown,
	}
}

func (capture *Capture) rotate(payloadLength int) {
	payloadFits := len(capture.active.payload)+payloadLength <= cap(capture.active.payload)
	recordFits := len(capture.active.records) < cap(capture.active.records)
	if payloadFits && recordFits {
		return
	}

	recordCapacity, payloadCapacity := replacementCapacities(
		len(capture.active.records),
		len(capture.active.payload),
	)
	if capture.next == nil {
		// This fallback is only expected if acquisition fills a chunk before the
		// background allocation of its successor completes.
		capture.next = newCaptureChunk(recordCapacity, payloadCapacity)
	}
	capture.active = capture.next
	capture.next = nil
	capture.chunks = append(capture.chunks, capture.active)
	capture.prepareNextLocked(recordCapacity, payloadCapacity)
}

// replacementCapacities sizes the next chunk from the fill shape of the chunk
// that caused rotation. The sealed chunk's record:payload ratio is preserved
// and scaled so that its proportionally fuller dimension returns to the
// initial capacity; the other dimension shrinks in proportion, floored at the
// minimum. Scaling back up to the initial capacities on every rotation keeps
// chunk sizes matched to observed traffic without ratcheting them down
// permanently after a transient traffic shape.
func replacementCapacities(records, payloadBytes int) (recordCapacity, payloadCapacity int) {
	recordCapacity = initialCaptureChunkRecordCapacity
	payloadCapacity = initialCaptureChunkPayloadCapacity
	if payloadBytes*initialCaptureChunkRecordCapacity >= records*initialCaptureChunkPayloadCapacity {
		if payloadBytes > 0 {
			recordCapacity = records * initialCaptureChunkPayloadCapacity / payloadBytes
		}
	} else {
		// records must be positive here, otherwise the payload branch was taken.
		payloadCapacity = payloadBytes * initialCaptureChunkRecordCapacity / records
	}
	return max(recordCapacity, minimumCaptureChunkRecordCapacity),
		max(payloadCapacity, minimumCaptureChunkPayloadCapacity)
}

// prepareNextLocked allocates the next chunk away from the append path. The
// caller must hold capture.mu.
func (capture *Capture) prepareNextLocked(recordCapacity, payloadCapacity int) {
	if capture.next != nil || capture.allocatingNext {
		return
	}

	capture.allocatingNext = true
	go func() {
		next := newCaptureChunk(recordCapacity, payloadCapacity)

		capture.mu.Lock()
		if capture.next == nil {
			capture.next = next
		}
		capture.allocatingNext = false
		capture.mu.Unlock()
	}()
}

// Append adds a frame event to the capture.
//
// Only the wall-clock reading of event.Timestamp is stored. Retrieved frames
// carry UTC timestamps with no monotonic reading, so a round-tripped event
// may not compare equal to the original even though the instants match.
func (capture *Capture) Append(event FrameEvent) error {
	if err := event.Validate(); err != nil {
		return err
	}

	payloadLength := event.Frame.DataLength()
	key := event.Key()

	capture.mu.Lock()
	defer capture.mu.Unlock()

	capture.rotate(payloadLength)
	chunk := capture.active

	keyState, ok := chunk.keyStates[key]
	if !ok {
		keyState = captureKeyState{
			index:      uint32(len(chunk.keys)),
			lastRecord: noCaptureRecord,
		}
		chunk.keys = append(chunk.keys, key)
	}
	payloadOffset := chunk.appendPayload(event.Frame.Data[:payloadLength])
	recordIndex := uint32(len(chunk.records))
	chunk.records = append(chunk.records, makeFrameRecord(
		event.Timestamp.UnixNano(),
		frameRecordData{
			payloadOffset: payloadOffset,
			keyIndex:      keyState.index,
			previous:      keyState.lastRecord,
			dlc:           event.Frame.DLC,
			flags:         event.Frame.Flags,
		},
	))
	keyState.lastRecord = recordIndex
	keyState.count++
	chunk.keyStates[key] = keyState

	capture.latest[key] = captureLocation{chunk: chunk, record: recordIndex}
	capture.length++
	// Decoding the record just written hands the waiters an owned copy, which
	// holds nothing of the chunk it came from.
	capture.wakeWaitersLocked(key, chunk.view().frameAt(recordIndex), Cursor{
		generation: capture.generation,
		chunk:      uint32(len(capture.chunks) - 1),
		record:     recordIndex,
	})
	return nil
}

// AppendEvent adds a non-frame event to the capture.
//
// Only the wall-clock reading of event.Timestamp is stored. Retrieved events
// carry UTC timestamps with no monotonic reading.
func (capture *Capture) AppendEvent(event Event) error {
	if err := event.Validate(); err != nil {
		return err
	}

	capture.mu.Lock()
	defer capture.mu.Unlock()

	capture.rotate(0)
	chunk := capture.active
	eventState, ok := chunk.eventStates[event.Bus]
	if !ok {
		eventState.lastRecord = noCaptureRecord
	}

	recordIndex := uint32(len(chunk.records))
	chunk.records = append(chunk.records, makeEventRecord(
		event.Timestamp.UnixNano(),
		eventRecordData{
			bus:              event.Bus,
			previous:         eventState.lastRecord,
			kind:             event.Kind,
			state:            event.ControllerState,
			txErrorCount:     event.TXErrorCount,
			rxErrorCount:     event.RXErrorCount,
			errorCountsKnown: event.ErrorCountsKnown,
		},
	))
	eventState.lastRecord = recordIndex
	eventState.count++
	chunk.eventStates[event.Bus] = eventState
	capture.length++
	return nil
}

// wakeWaitersLocked hands the record that woke the waiters to every Next
// parked on key, or a zero cursor when Clear discarded their position. The
// waiter leaves the map first, so only the first matching append after parking
// can set it, which is the earliest record those callers have not seen.
func (capture *Capture) wakeWaitersLocked(key FrameKey, event FrameEvent, cursor Cursor) {
	waiter := capture.waiters[key]
	if waiter == nil {
		return
	}
	delete(capture.waiters, key)
	waiter.event, waiter.cursor = event, cursor
	close(waiter.ready)
}

// Latest returns an owned copy of the latest frame matching key.
func (capture *Capture) Latest(key FrameKey) (FrameEvent, bool) {
	capture.mu.RLock()
	defer capture.mu.RUnlock()

	location, ok := capture.latest[key]
	if !ok {
		return FrameEvent{}, false
	}
	return location.chunk.view().frameAt(location.record), true
}

// End returns the cursor at the current end of the capture. A caller can pass
// it to Next or any Since method to observe only later records.
func (capture *Capture) End() Cursor {
	capture.mu.RLock()
	defer capture.mu.RUnlock()
	return capture.endLocked()
}

func (capture *Capture) endLocked() Cursor {
	for chunk := len(capture.chunks) - 1; chunk >= 0; chunk-- {
		if records := len(capture.chunks[chunk].records); records > 0 {
			return Cursor{
				generation: capture.generation,
				chunk:      uint32(chunk),
				record:     uint32(records - 1),
			}
		}
	}
	return Cursor{}
}

// Next returns the first frame matching key after cursor. It returns
// immediately when that frame is already retained and otherwise waits for a
// matching append or context cancellation.
//
// Pass End() to wait only for a frame that has not arrived yet. Pass an older
// cursor to walk forward through retained history. The returned cursor marks
// the returned frame, not the capture's current end.
//
// Next is built for following a frontier: each call costs the key's own
// traffic since cursor, however busy the rest of the capture is. To step
// through deep history, read in bulk with SeriesSince and continue from the
// returned cursor.
//
// A cursor the capture cannot place, including one held across a Clear that
// happens while Next waits, returns cursor unchanged with
// ErrCursorOutOfRange.
func (capture *Capture) Next(ctx context.Context, key FrameKey, cursor Cursor) (FrameEvent, Cursor, error) {
	for {
		// The walk to the snapshot's end reads write-once history, so as in the
		// Since reads only capturing the snapshot holds the read lock.
		capture.mu.RLock()
		snapshot := capture.nextSearchLocked(key, cursor)
		capture.mu.RUnlock()
		if snapshot.err != nil {
			return FrameEvent{}, cursor, snapshot.err
		}
		if event, next, ok := snapshot.find(); ok {
			return event, next, nil
		}

		// Nothing matched. Register the waiter and re-walk in one critical
		// section, so an append lands either inside that walk or after the
		// waiter can hear it, never between the two.
		waiter, delta := capture.addWaiter(key, cursor)
		if delta.err != nil {
			return FrameEvent{}, cursor, delta.err
		}
		if event, next, ok := delta.find(); ok {
			capture.removeWaiter(key, waiter)
			return event, next, nil
		}

		select {
		case <-waiter.ready:
			// An append wake carries the frame it appended, the earliest this
			// call has not seen. A Clear wake carries nothing, so the next pass
			// reports the discarded cursor, or waits again if it still places.
			if waiter.cursor != (Cursor{}) {
				return waiter.event, waiter.cursor, nil
			}
		case <-ctx.Done():
			capture.removeWaiter(key, waiter)
			return FrameEvent{}, cursor, ctx.Err()
		}
	}
}

// nextSearch freezes the state one Next walk scans after the lock is
// released. Every chunk but the last is sealed, so their views and key states
// may be read lazily without the lock; the active chunk's view and state were
// frozen while it was held. start and boundary place the cursor in the
// snapshot; err reports a cursor the snapshot cannot place, and leaves the
// walk unusable.
type nextSearch struct {
	key         FrameKey
	generation  uint64
	chunks      []*captureChunk
	activeView  captureView
	activeState captureKeyState
	start       int
	boundary    int
	err         error
}

// nextSearchLocked captures what one Next walk from cursor needs. The caller
// must hold at least the read lock.
func (capture *Capture) nextSearchLocked(key FrameKey, cursor Cursor) nextSearch {
	search := nextSearch{
		key:         key,
		generation:  capture.generation,
		chunks:      capture.chunks,
		activeView:  capture.active.view(),
		activeState: capture.active.keyStates[key],
	}
	search.start, search.boundary, search.err = locateCursor(cursor, search.generation, search.chunks)
	return search
}

func (search nextSearch) chunkAt(index int) (captureView, captureKeyState) {
	if index == len(search.chunks)-1 {
		return search.activeView, search.activeState
	}
	chunk := search.chunks[index]
	return chunk.view(), chunk.keyStates[search.key]
}

// find returns the first frame matching the search key after its cursor, up
// to the snapshot's end.
func (search nextSearch) find() (FrameEvent, Cursor, bool) {
	start, boundary := search.start, search.boundary
	for chunkIndex := start; chunkIndex < len(search.chunks); chunkIndex++ {
		view, state := search.chunkAt(chunkIndex)
		if state.count == 0 {
			continue
		}

		stop := -1
		if chunkIndex == start {
			stop = boundary
		}
		// The backward walk from the chunk's series tail visits only records
		// of this key, so the cost of following a frontier scales with the
		// key's own traffic since the cursor, not with everything else
		// appended in between. The earliest record visited before crossing
		// stop is the first match.
		first := noCaptureRecord
		for r := state.lastRecord; r != noCaptureRecord && int(r) > stop; r = view.records[r].frameData().previous {
			first = r
		}
		if first == noCaptureRecord {
			continue
		}

		next := Cursor{
			generation: search.generation,
			chunk:      uint32(chunkIndex),
			record:     first,
		}
		return view.frameAt(first), next, true
	}
	return FrameEvent{}, Cursor{}, false
}

// addWaiter registers one waiter for key and captures the search state from
// cursor in the same critical section, so an append can land only inside the
// returned delta or after the waiter is able to hear it, never between.
func (capture *Capture) addWaiter(key FrameKey, cursor Cursor) (*captureWaiter, nextSearch) {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	search := capture.nextSearchLocked(key, cursor)
	if search.err != nil {
		return nil, search
	}
	waiter := capture.waiters[key]
	if waiter == nil {
		if capture.waiters == nil {
			capture.waiters = make(map[FrameKey]*captureWaiter)
		}
		waiter = &captureWaiter{ready: make(chan struct{})}
		capture.waiters[key] = waiter
	}
	waiter.count++
	return waiter, search
}

func (capture *Capture) removeWaiter(key FrameKey, waiter *captureWaiter) {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	if capture.waiters[key] != waiter {
		return
	}
	waiter.count--
	if waiter.count == 0 {
		delete(capture.waiters, key)
	}
}

// Frames returns owned copies of every captured frame in append order.
func (capture *Capture) Frames() []FrameEvent {
	// The zero Cursor is always placeable, so this read cannot fail.
	frames, _, _ := capture.FramesSince(Cursor{})
	return frames
}

// WriteRecordsSince sends every frame and event appended after cursor to
// writer in Capture append order. On success it returns the cursor of the
// newest record observed. On failure it returns the cursor of the last record
// whose writer call succeeded and a *RecordWriteError whose Cursor identifies
// the failed record. Retry from the returned cursor, or deliberately skip the
// failed record by continuing from the error's Cursor.
//
// A writer may commit partial output before returning an error. Capture tracks
// completed writer calls, not the transactional state of the underlying
// output. Pass the zero Cursor to write the full retained Capture.
func (capture *Capture) WriteRecordsSince(cursor Cursor, writer RecordWriter) (Cursor, error) {
	views, skip, next, err := capture.viewsSince(cursor)
	if err != nil {
		return cursor, err
	}
	startChunk := uint32(0)
	if cursor != (Cursor{}) {
		startChunk = cursor.chunk
	}
	return writeRecords(views, skip, startChunk, cursor, next, writer)
}

func writeRecords(
	views []captureView,
	skip int,
	firstChunk uint32,
	lastWritten Cursor,
	success Cursor,
	writer RecordWriter,
) (Cursor, error) {
	for i := range views {
		from := 0
		if i == 0 {
			from = skip
		}
		for record := from; record < len(views[i].records); record++ {
			var err error
			switch views[i].records[record].kind {
			case captureRecordFrame:
				err = writer.WriteFrame(views[i].frameAt(uint32(record)))
			case captureRecordEvent:
				err = writer.WriteEvent(views[i].eventAt(uint32(record)))
			default:
				panic("gocan: capture record has an unknown kind")
			}
			recordCursor := Cursor{
				generation: success.generation,
				chunk:      firstChunk + uint32(i),
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

// FramesSince returns owned copies of every frame appended after cursor, in
// append order, together with the cursor of the newest record observed. Pass
// the returned cursor to a later Since read to receive only what arrived in
// between; pass the zero Cursor to start from the beginning.
func (capture *Capture) FramesSince(cursor Cursor) ([]FrameEvent, Cursor, error) {
	views, skip, next, err := capture.viewsSince(cursor)
	if err != nil {
		return nil, cursor, err
	}
	return framesFromViews(views, skip), next, nil
}

func framesFromViews(views []captureView, skip int) []FrameEvent {
	total := recordKindCount(views, skip, captureRecordFrame)
	if total <= 0 {
		return nil
	}

	frames := make([]FrameEvent, 0, total)
	for i := range views {
		from := 0
		if i == 0 {
			from = skip
		}
		for record := from; record < len(views[i].records); record++ {
			if views[i].records[record].kind == captureRecordFrame {
				frames = append(frames, views[i].frameAt(uint32(record)))
			}
		}
	}
	return frames
}

// Events returns every captured non-frame event in append order.
func (capture *Capture) Events() []Event {
	// The zero Cursor is always placeable, so this read cannot fail.
	events, _, _ := capture.EventsSince(Cursor{})
	return events
}

// EventsSince returns every non-frame event appended after cursor, in append
// order, together with the cursor of the newest record observed. Pass the
// returned cursor to a later Since read to receive only what arrived in
// between; pass the zero Cursor to start from the beginning.
func (capture *Capture) EventsSince(cursor Cursor) ([]Event, Cursor, error) {
	views, skip, next, err := capture.viewsSince(cursor)
	if err != nil {
		return nil, cursor, err
	}
	return eventsFromViews(views, skip), next, nil
}

func eventsFromViews(views []captureView, skip int) []Event {
	total := recordKindCount(views, skip, captureRecordEvent)
	if total <= 0 {
		return nil
	}

	events := make([]Event, 0, total)
	for i := range views {
		from := 0
		if i == 0 {
			from = skip
		}
		for record := from; record < len(views[i].records); record++ {
			if views[i].records[record].kind == captureRecordEvent {
				events = append(events, views[i].eventAt(uint32(record)))
			}
		}
	}
	return events
}

func (capture *Capture) viewsSince(cursor Cursor) ([]captureView, int, Cursor, error) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	generation := capture.generation
	capture.mu.RUnlock()

	// The placed cursor names its own chunk, so the read jumps straight to it
	// and its cost scales with what arrived since the cursor, not with total
	// capture history.
	start, boundary, err := locateCursor(cursor, generation, chunks)
	if err != nil {
		return nil, 0, cursor, err
	}
	skip := boundary + 1
	views := make([]captureView, len(chunks)-start)
	for i := range views {
		if start+i == len(chunks)-1 {
			views[i] = activeView
		} else {
			views[i] = chunks[start+i].view()
		}
	}

	next := cursor
	for i := len(views) - 1; i >= 0; i-- {
		if count := len(views[i].records); count > 0 {
			next = Cursor{generation: generation, chunk: uint32(start + i), record: uint32(count - 1)}
			break
		}
	}

	return views, skip, next, nil
}

func recordKindCount(views []captureView, skip int, kind captureRecordKind) int {
	total := 0
	for i := range views {
		from := 0
		if i == 0 {
			from = skip
		}
		for record := from; record < len(views[i].records); record++ {
			if views[i].records[record].kind == kind {
				total++
			}
		}
	}
	return total
}

// Series returns owned copies of every captured frame matching key in append
// order.
func (capture *Capture) Series(key FrameKey) []FrameEvent {
	// The zero Cursor is always placeable, so this read cannot fail.
	frames, _, _ := capture.SeriesSince(key, Cursor{})
	return frames
}

// SeriesSince returns owned copies of every frame matching key appended after
// cursor, in append order, together with the cursor of the newest record
// observed anywhere in the capture. Pass the returned cursor to a later Since
// read to receive only what arrived in between; pass the zero Cursor to start
// from the beginning.
func (capture *Capture) SeriesSince(key FrameKey, cursor Cursor) ([]FrameEvent, Cursor, error) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	activeState := capture.active.keyStates[key]
	generation := capture.generation
	capture.mu.RUnlock()

	// The placed cursor names its own chunk, so the read jumps straight to it
	// and its cost scales with what arrived since the cursor, not with total
	// capture history.
	start, boundary, err := locateCursor(cursor, generation, chunks)
	if err != nil {
		return nil, cursor, err
	}
	views := make([]captureView, len(chunks)-start)
	states := make([]captureKeyState, len(views))
	for i := range views {
		chunkIndex := start + i
		if chunkIndex == len(chunks)-1 {
			views[i], states[i] = activeView, activeState
		} else {
			views[i] = chunks[chunkIndex].view()
			states[i] = chunks[chunkIndex].keyStates[key]
		}
	}

	// The returned cursor is the newest record observed anywhere, not the
	// (possibly much older) series tail, so cursors remain global positions
	// and never move backwards: an input cursor always lies at or before the
	// capture's current tail.
	next := cursor
	for i := len(views) - 1; i >= 0; i-- {
		if count := len(views[i].records); count > 0 {
			next = Cursor{generation: generation, chunk: uint32(start + i), record: uint32(count - 1)}
			break
		}
	}

	// Record indexes within a chunk follow append order and prevByKey links
	// strictly decrease, so the cursor's position is a walk stop condition:
	// within its chunk every record index at or below cursor.record is
	// excluded, and earlier chunks are excluded entirely.
	total := 0
	for i := range views {
		if states[i].count == 0 {
			continue
		}
		if i == 0 && boundary >= 0 {
			for r := states[i].lastRecord; r != noCaptureRecord && int(r) > boundary; r = views[i].records[r].frameData().previous {
				total++
			}
		} else {
			total += int(states[i].count)
		}
	}
	if total == 0 {
		return nil, next, nil
	}

	// The counts size the result exactly, and the backward walks from each
	// chunk's captured tail fill it newest-first, which restores append order
	// without a reversal pass.
	frames := make([]FrameEvent, total)
	remaining := total
	for i := len(views) - 1; i >= 0; i-- {
		if states[i].count == 0 {
			continue
		}
		stop := -1
		if i == 0 {
			stop = boundary
		}
		for r := states[i].lastRecord; r != noCaptureRecord && int(r) > stop; r = views[i].records[r].frameData().previous {
			remaining--
			frames[remaining] = views[i].frameAt(r)
		}
	}
	return frames, next, nil
}

// BusEvents returns every captured event from bus in append order.
func (capture *Capture) BusEvents(bus BusID) []Event {
	// The zero Cursor is always placeable, so this read cannot fail.
	events, _, _ := capture.BusEventsSince(bus, Cursor{})
	return events
}

// BusEventsSince returns every event from bus appended after cursor, in append
// order, together with the cursor of the newest record observed anywhere in
// the capture. Pass the returned cursor to a later Since read to receive only
// what arrived in between; pass the zero Cursor to start from the beginning.
func (capture *Capture) BusEventsSince(bus BusID, cursor Cursor) ([]Event, Cursor, error) {
	capture.mu.RLock()
	chunks := capture.chunks
	activeView := capture.active.view()
	activeState := capture.active.eventStates[bus]
	generation := capture.generation
	capture.mu.RUnlock()

	start, boundary, err := locateCursor(cursor, generation, chunks)
	if err != nil {
		return nil, cursor, err
	}
	views := make([]captureView, len(chunks)-start)
	states := make([]captureEventState, len(views))
	for i := range views {
		chunkIndex := start + i
		if chunkIndex == len(chunks)-1 {
			views[i], states[i] = activeView, activeState
		} else {
			views[i] = chunks[chunkIndex].view()
			states[i] = chunks[chunkIndex].eventStates[bus]
		}
	}

	next := cursor
	for i := len(views) - 1; i >= 0; i-- {
		if count := len(views[i].records); count > 0 {
			next = Cursor{generation: generation, chunk: uint32(start + i), record: uint32(count - 1)}
			break
		}
	}

	total := 0
	for i := range views {
		if states[i].count == 0 {
			continue
		}
		if i == 0 && boundary >= 0 {
			for record := states[i].lastRecord; record != noCaptureRecord && int(record) > boundary; record = views[i].records[record].eventData().previous {
				total++
			}
		} else {
			total += int(states[i].count)
		}
	}
	if total == 0 {
		return nil, next, nil
	}

	events := make([]Event, total)
	remaining := total
	for i := len(views) - 1; i >= 0; i-- {
		if states[i].count == 0 {
			continue
		}
		stop := -1
		if i == 0 {
			stop = boundary
		}
		for record := states[i].lastRecord; record != noCaptureRecord && int(record) > stop; record = views[i].records[record].eventData().previous {
			remaining--
			events[remaining] = views[i].eventAt(record)
		}
	}
	return events, next, nil
}

// Len returns the number of retained frame and event records.
func (capture *Capture) Len() int {
	capture.mu.RLock()
	defer capture.mu.RUnlock()
	return capture.length
}

// Clear discards all retained records and indexes. Readers already in progress
// finish against their existing snapshot. A Next waiting on a cursor this
// Clear discards wakes and reports ErrCursorOutOfRange instead of waiting for
// unrelated traffic to reveal the reset.
func (capture *Capture) Clear() {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	fresh := capture.next
	if fresh == nil {
		fresh = newCaptureChunk(cap(capture.active.records), cap(capture.active.payload))
	}
	capture.generation = captureEpochs.Add(1)
	capture.next = nil
	capture.active = fresh
	capture.chunks = []*captureChunk{fresh}
	clear(capture.latest)
	capture.length = 0
	capture.prepareNextLocked(cap(fresh.records), cap(fresh.payload))

	// Every waiter is parked on a position this Clear just discarded. Waking
	// them makes each Next re-walk against the new generation, where it either
	// reports the loss or, for a caller reading from the beginning, waits on
	// the fresh history.
	for key := range capture.waiters {
		capture.wakeWaitersLocked(key, FrameEvent{}, Cursor{})
	}
}
