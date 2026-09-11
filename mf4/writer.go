// Package mf4 records raw CAN/CAN FD bus events in MDF 4.10 files.
//
// The bus structures and attachment links follow ASAM MDF Bus Logging, as
// used by python-can's can/io/mf4.py and asammdf (see .repos/.lock).
// https://www.asam.net/standards/detail/mdf/wiki/ describes the block model.
// Non-frame Capture observations are exported as timestamped event markers.
// This package does not write decoded channels, recover interrupted files,
// or append to existing measurements.
//
// To preserve captures from clocks with limited resolution, Writer accepts
// equal encoded timestamps within a time master. This relaxes MDF 4.1's strict
// ordering requirement; readers enforcing it may reject such files.
package mf4

import (
	"bytes"
	"compress/zlib"
	"crypto/md5" // MDF attachment checksum, not a security primitive.
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tomrford/gocan"
)

const bufferSize = 64 << 10

// Database associates a DBC description with one logical bus. Data can come
// from []byte(db.Source()) or an original DBC file. The writer embeds these
// bytes without interpreting or converting their encoding.
type Database struct {
	Bus  gocan.BusID
	Name string
	Data []byte
}

// Options configures an MF4 export. The zero value writes uncompressed data.
type Options struct {
	Databases []Database
	// Compression applies Deflate to each chunk of frame records.
	Compression bool
	// ToolName and ToolVersion identify the exporting application in file
	// history. An empty name defaults to gocan; an empty version stays blank.
	ToolName, ToolVersion string
	// Comment describes the export. Properties holds application-defined
	// metadata, such as workspace or configuration identifiers. Keys must be
	// nonempty. All metadata text must be valid UTF-8 and XML 1.0 text.
	Comment    string
	Properties map[string]string
	// BusNames supplies optional display names. Empty names use CAN<BusID>.
	// Numeric bus identity and canonical source paths remain unchanged.
	BusNames map[gocan.BusID]string
}

// Writer streams frames and event markers to an initially empty seekable file.
// It buffers at most 64 KiB of frame records, plus compression workspace when
// enabled. Retained metadata grows with buses, not frames or event markers.
// Buses need not be declared in advance. Writer is not safe for concurrent use.
//
// Flush delivers accepted frames, but the file remains marked unfinished until
// Close writes the final counters and clears that marker. Neither operation
// closes or syncs the underlying file. Successful finalisation is distinct from
// durable storage and from independent conformance validation. Recovery after
// an interrupted write or failed Close is unsupported, including after Flush.
// Output/seek failures are sticky; validation errors leave prior frames usable.
type Writer struct {
	output      io.WriteSeeker
	start       time.Time
	end         int64
	err         error
	closed      bool
	buffer      []byte
	compressor  *zlib.Writer
	compressed  bytes.Buffer
	groups      map[uint32]*channelGroup
	attachments map[gocan.BusID]uint64
	groupLink   int64
	dataLink    int64
	dataOffset  uint64
	eventLink   int64
	busNames    map[gocan.BusID]string
}

type channelGroup struct {
	address  uint64
	id       uint32
	cycles   uint64
	lastTime float64
}

var _ gocan.RecordWriter = (*Writer)(nil)

// NewWriter derives start from the first accepted frame or event, falling back
// to file creation time for empty exports. Timestamps must be between the Unix
// epoch and the end of Go's int64 nanosecond range. Frames and events
// before start are rejected. Encoded timestamps within one bus and frame kind
// (data or remote) must not regress. Equal timestamps are retained, with the
// conformance limitation described in the package documentation. Accepted
// frames retain append order.
// Times are stored as float64 seconds relative to start, with its UTC epoch
// nanoseconds in the header. Relative time precision decreases for long runs.
// Each bus may have one DBC attachment, linked from both frame structures.
// Event markers retain their own append order separately from frame records.
func NewWriter(output io.WriteSeeker, options Options) (*Writer, error) {
	if output == nil {
		return nil, errors.New("MF4 writer requires an output")
	}
	headerComment, historyComment, err := options.comments()
	if err != nil {
		return nil, err
	}
	seen := make(map[gocan.BusID]bool)
	for _, database := range options.Databases {
		if database.Bus == 0 || seen[database.Bus] {
			return nil, fmt.Errorf("invalid or duplicate DBC bus %d", database.Bus)
		}
		if database.Name == "" || path.Base(database.Name) != database.Name || strings.ContainsAny(database.Name, "\\\x00") ||
			!strings.EqualFold(path.Ext(database.Name), ".dbc") || !utf8.ValidString(database.Name) || len(database.Data) == 0 {
			return nil, fmt.Errorf("bus %d requires a UTF-8 basename ending in .dbc and nonempty data", database.Bus)
		}
		seen[database.Bus] = true
	}
	for bus, name := range options.BusNames {
		if bus == 0 || !validXMLText(name) {
			return nil, fmt.Errorf("invalid MF4 display name or bus ID for bus %d", bus)
		}
	}
	size, err := output.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size != 0 {
		return nil, errors.New("MF4 output must be empty")
	}
	w := &Writer{output: output, buffer: make([]byte, 0, bufferSize),
		groups: make(map[uint32]*channelGroup), attachments: make(map[gocan.BusID]uint64),
		eventLink: 64 + 56, busNames: maps.Clone(options.BusNames)}
	if options.Compression {
		w.compressor = zlib.NewWriter(&w.compressed)
	}
	id := make([]byte, 64)
	copy(id, "UnFinMF ")
	copy(id[8:], "4.10    ")
	copy(id[16:], "gocan   ")
	binary.LittleEndian.PutUint16(id[28:], 410)
	binary.LittleEndian.PutUint16(id[60:], 1) // CG cycle counters need finalisation.
	w.write(id)
	created := uint64(time.Now().UnixNano())
	header := make([]byte, 32)
	binary.LittleEndian.PutUint64(header, created)
	w.block("##HD", make([]uint64, 6), header)
	if headerComment != "" {
		w.patch64(64+64, w.textBlock("##MD", headerComment))
	}
	// MDF requires a creation history entry and its structured XML comment,
	// even though permissive readers can open files without either.
	comment := w.textBlock("##MD", historyComment)
	history := make([]byte, 16)
	binary.LittleEndian.PutUint64(history, created)
	fh := w.block("##FH", []uint64{0, comment}, history)
	w.patch64(64+32, fh)
	dg := w.block("##DG", make([]uint64, 4), []byte{4, 0, 0, 0, 0, 0, 0, 0})
	w.patch64(64+24, dg)
	w.groupLink = int64(dg) + 32
	w.dataLink = int64(dg) + 40
	attachmentLink := int64(64 + 48)
	for _, database := range options.Databases {
		name := w.text(database.Name)
		mime := w.text("application/x-dbc")
		data := make([]byte, 40+len(database.Data))
		binary.LittleEndian.PutUint16(data, 5) // embedded, MD5 valid
		hash := md5.Sum(database.Data)
		copy(data[8:], hash[:])
		binary.LittleEndian.PutUint64(data[24:], uint64(len(database.Data)))
		binary.LittleEndian.PutUint64(data[32:], uint64(len(database.Data)))
		copy(data[40:], database.Data)
		at := w.block("##AT", []uint64{0, name, mime, 0}, data)
		w.patch64(attachmentLink, at)
		attachmentLink = int64(at) + 24
		w.attachments[database.Bus] = at
	}
	if w.err != nil {
		return nil, w.err
	}
	return w, nil
}

// WriteFrame accepts a classical data, CAN FD, or remote frame. BusChannel is
// stored as uint16, preserving the full gocan.BusID range without renumbering.
func (w *Writer) WriteFrame(event gocan.FrameEvent) error {
	if err := w.ready(); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	offset, err := w.timeOffset(event.Timestamp)
	if err != nil {
		return err
	}
	remote := event.Frame.Flags.Has(gocan.FrameRemote)
	key := uint32(event.Bus) << 1
	if remote {
		key++
	}
	group := w.groups[key]
	seconds := float64(offset) / 1e9
	if group != nil && group.cycles != 0 && seconds < group.lastTime {
		return fmt.Errorf("MF4 bus %d time master must not regress", event.Bus)
	}
	if group == nil {
		group = w.addGroup(event.Bus, remote)
		if w.err != nil {
			return w.err
		}
		w.groups[key] = group
	}
	// Four-byte record ID followed by time and the bus-event structure.
	var record [89]byte
	binary.LittleEndian.PutUint32(record[:], group.id)
	binary.LittleEndian.PutUint64(record[4:], math.Float64bits(seconds))
	binary.LittleEndian.PutUint16(record[12:], uint16(event.Bus))
	binary.LittleEndian.PutUint32(record[14:], event.Frame.ID)
	record[18] = flag(event.Frame, gocan.FrameExtended)
	record[19] = event.Frame.DLC
	record[20] = byte(event.Frame.DataLength())
	size := len(record)
	directionOffset := 85
	if remote {
		size = 22
		directionOffset = 21
	} else {
		copy(record[21:85], event.Frame.Data[:event.Frame.DataLength()])
		record[86] = flag(event.Frame, gocan.FrameFD)
		record[87] = flag(event.Frame, gocan.FrameBitRateSwitch)
		record[88] = flag(event.Frame, gocan.FrameErrorStateIndicator)
	}
	if event.Direction == gocan.DirectionTransmit {
		record[directionOffset] = 1
	}
	if len(w.buffer)+size > cap(w.buffer) {
		if err := w.Flush(); err != nil {
			return err
		}
	}
	w.buffer = append(w.buffer, record[:size]...)
	group.cycles++
	group.lastTime = seconds
	return nil
}

// WriteEvent exports a Capture observation as a file-level point marker.
// Its name and comment identify the bus, including buses with no frames.
// Markers may share timestamps; frame-only readers may not expose them.
func (w *Writer) WriteEvent(event gocan.Event) error {
	if err := w.ready(); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	var name, details string
	switch event.Kind {
	case gocan.EventControllerState:
		states := [...]string{"", "active", "warning", "passive", "bus_off"}
		name = "controller state"
		details = fmt.Sprintf("ControllerState=%s; ErrorCountsKnown=%t", states[event.ControllerState], event.ErrorCountsKnown)
		if event.ErrorCountsKnown {
			details += fmt.Sprintf("; TXErrorCount=%d; RXErrorCount=%d", event.TXErrorCount, event.RXErrorCount)
		}
	case gocan.EventErrorFrame:
		name = "error observation"
	case gocan.EventReceiveOverrun:
		name = "receive overrun"
	default:
		return fmt.Errorf("MF4 does not support Capture event kind %d", event.Kind)
	}
	offset, err := w.timeOffset(event.Timestamp)
	if err != nil {
		return err
	}
	busName := fmt.Sprintf("CAN%d", event.Bus)
	if label := w.busNames[event.Bus]; label != "" {
		busName += " (" + label + ")"
	}
	title := w.text(busName + " " + name)
	var comment uint64
	if details != "" {
		// Only validated numbers and fixed labels enter this XML text.
		comment = w.textBlock("##MD", fmt.Sprintf(`<EVcomment xmlns="http://www.asam.net/mdf/v4"><TX>Bus=%d; %s</TX></EVcomment>`, event.Bus, details))
	}
	data := make([]byte, 32)
	data[0], data[1] = 6, 1 // marker, time sync
	binary.LittleEndian.PutUint64(data[16:], uint64(offset))
	binary.LittleEndian.PutUint64(data[24:], math.Float64bits(1e-9))
	ev := w.block("##EV", []uint64{0, 0, 0, title, comment}, data)
	w.patch64(w.eventLink, ev)
	w.eventLink = int64(ev) + 24
	return w.err
}

// Flush delivers buffered frames to output. The file still requires Close.
func (w *Writer) Flush() error {
	if w.err != nil {
		return w.err
	}
	if len(w.buffer) == 0 {
		return nil
	}
	var address uint64
	if w.compressor == nil {
		address = w.block("##DT", nil, w.buffer)
	} else {
		w.compressed.Reset()
		w.compressed.Write(make([]byte, 24)) // DZ parameters precede the zlib stream.
		w.compressor.Reset(&w.compressed)
		// The compressor writes to a bytes.Buffer, which cannot return an error.
		w.compressor.Write(w.buffer)
		w.compressor.Close()
		data := w.compressed.Bytes()
		copy(data, "DT") // original block type; plain Deflate, no transposition
		binary.LittleEndian.PutUint64(data[8:], uint64(len(w.buffer)))
		binary.LittleEndian.PutUint64(data[16:], uint64(len(data)-24))
		address = w.block("##DZ", nil, data)
	}
	// One variable-sized data block per list. Offsets refer to the logical
	// concatenation of all DT payloads, excluding alignment padding.
	data := make([]byte, 16)
	binary.LittleEndian.PutUint32(data[4:], 1)
	binary.LittleEndian.PutUint64(data[8:], w.dataOffset)
	dl := w.block("##DL", []uint64{0, address}, data)
	link := dl
	if w.compressor != nil && w.dataOffset == 0 {
		link = w.block("##HL", []uint64{dl}, make([]byte, 8)) // variable-sized Deflate chunks
	}
	w.patch64(w.dataLink, link)
	if w.err != nil {
		return w.err
	}
	w.dataLink = int64(dl) + 24
	w.dataOffset += uint64(len(w.buffer))
	w.buffer = w.buffer[:0]
	return nil
}

// Close flushes records and finalises counters and the file identifier. It is
// idempotent, including after a failure, and leaves output open for Sync/Close.
func (w *Writer) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if err := w.Flush(); err != nil {
		return err
	}
	for _, group := range w.groups {
		w.patch64(int64(group.address)+80, group.cycles)
	}
	w.patch(60, []byte{0, 0})
	// Last write publishes a finalised file only after every other patch.
	w.patch(0, []byte("MDF     "))
	return w.err
}

func (w *Writer) ready() error {
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return errors.New("MF4 writer is closed")
	}
	return nil
}

func (w *Writer) timeOffset(timestamp time.Time) (int64, error) {
	if !validTime(timestamp) {
		return 0, errors.New("MF4 timestamp is outside the supported nanosecond range")
	}
	if w.start.IsZero() {
		w.patch64(64+24+6*8, uint64(timestamp.UnixNano())) // HD data starts after six links.
		if w.err != nil {
			return 0, w.err
		}
		w.start = timestamp
	}
	if timestamp.UnixNano() < w.start.UnixNano() {
		return 0, errors.New("MF4 timestamp is before start")
	}
	return timestamp.UnixNano() - w.start.UnixNano(), nil
}

func validTime(t time.Time) bool {
	return !t.IsZero() && t.Unix() >= 0 && time.Unix(0, t.UnixNano()).Equal(t)
}

func flag(frame gocan.Frame, bit gocan.FrameFlags) byte {
	if frame.Flags.Has(bit) {
		return 1
	}
	return 0
}

func (w *Writer) addGroup(bus gocan.BusID, remote bool) *channelGroup {
	name := "CAN_DataFrame"
	structureSize := uint32(77)
	if remote {
		name = "CAN_RemoteFrame"
		structureSize = 10
	}
	busPath := w.text(fmt.Sprintf("CAN%d", bus))
	busName := busPath
	if label := w.busNames[bus]; label != "" {
		busName = w.text(label)
	}
	source := w.block("##SI", []uint64{busName, busPath, 0}, []byte{2, 2, 0, 0, 0, 0, 0, 0})
	// CN components use record-relative offsets, including the time master.
	type member struct {
		name         string
		offset, bits uint32
		kind         byte
	}
	fields := []member{
		{"BusChannel", 8, 16, 0}, {"ID", 10, 32, 0}, {"IDE", 14, 8, 0},
		{"DLC", 15, 8, 0}, {"DataLength", 16, 8, 0},
	}
	if remote {
		fields = append(fields, member{"Dir", 17, 8, 0})
	} else {
		fields = append(fields, []member{
			{"DataBytes", 17, 512, 10}, {"Dir", 81, 8, 0}, {"EDL", 82, 8, 0}, {"BRS", 83, 8, 0}, {"ESI", 84, 8, 0},
		}...)
	}
	var next uint64
	for i := len(fields) - 1; i >= 0; i-- {
		field := fields[i]
		next = w.channel(name+"."+field.name, next, 0, 0, 0, field.offset, field.bits, field.kind, false)
	}
	structure := w.channel(name, 0, next, source, w.attachments[bus], 8, structureSize*8, 10, false)
	master := w.channel("time", structure, 0, 0, 0, 0, 64, 4, true)
	group := &channelGroup{id: uint32(len(w.groups) + 1)}
	data := make([]byte, 32)
	binary.LittleEndian.PutUint64(data, uint64(group.id))
	binary.LittleEndian.PutUint16(data[16:], 6) // bus event, plain bus event
	binary.LittleEndian.PutUint16(data[18:], '.')
	binary.LittleEndian.PutUint32(data[24:], structureSize+8)
	group.address = w.block("##CG", []uint64{0, master, w.text(name), source, 0, 0}, data)
	w.patch64(w.groupLink, group.address)
	w.groupLink = int64(group.address) + 24
	return group
}

func (w *Writer) channel(name string, next, component, source, attachment uint64, offset, bits uint32, kind byte, master bool) uint64 {
	links := []uint64{next, component, w.text(name), source, 0, 0, 0, 0}
	data := make([]byte, 72)
	data[2] = kind
	binary.LittleEndian.PutUint32(data[4:], offset)
	binary.LittleEndian.PutUint32(data[8:], bits)
	if master {
		data[0], data[1] = 2, 1 // time master
		links[6] = w.text("s")
	} else {
		binary.LittleEndian.PutUint32(data[12:], 1<<10) // bus-event channel
	}
	if attachment != 0 {
		links = append(links, attachment)
		binary.LittleEndian.PutUint16(data[22:], 1)
	}
	return w.block("##CN", links, data)
}

func (w *Writer) text(value string) uint64 {
	return w.textBlock("##TX", value)
}

func (w *Writer) textBlock(kind, value string) uint64 {
	data := append([]byte(value), 0)
	// Text block lengths include zero padding; DT/AT lengths exclude padding.
	data = append(data, make([]byte, (-len(data))&7)...)
	return w.block(kind, nil, data)
}

func (w *Writer) block(kind string, links []uint64, data []byte) uint64 {
	w.write(make([]byte, (-w.end)&7))
	address := uint64(w.end)
	header := make([]byte, 24+8*len(links))
	copy(header, kind)
	binary.LittleEndian.PutUint64(header[8:], uint64(len(header)+len(data)))
	binary.LittleEndian.PutUint64(header[16:], uint64(len(links)))
	for i, link := range links {
		binary.LittleEndian.PutUint64(header[24+8*i:], link)
	}
	w.write(header)
	w.write(data)
	return address
}

func (w *Writer) write(data []byte) {
	if w.err != nil {
		return
	}
	n, err := w.output.Write(data)
	w.end += int64(n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	w.err = err
}

func (w *Writer) patch64(offset int64, value uint64) {
	var data [8]byte
	binary.LittleEndian.PutUint64(data[:], value)
	w.patch(offset, data[:])
}

func (w *Writer) patch(offset int64, data []byte) {
	if w.err != nil {
		return
	}
	if _, w.err = w.output.Seek(offset, io.SeekStart); w.err != nil {
		return
	}
	n, err := w.output.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
		return
	}
	_, w.err = w.output.Seek(w.end, io.SeekStart)
}
