package mf4_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/dbc"
	"github.com/tomrford/gocan/mf4"
	"github.com/tomrford/gocan/recorder"
)

var start = time.Date(2026, 9, 6, 10, 0, 0, 123456789, time.UTC)

type image []byte

func (file image) u64(offset uint64) uint64 {
	return binary.LittleEndian.Uint64(file[offset:])
}

func (file image) text(addr uint64) string {
	return strings.TrimRight(string(file[addr+24:addr+file.u64(addr+8)]), "\x00")
}

func readImage(t *testing.T, f *os.File) image {
	t.Helper()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func newFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "capture.mf4"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func frame(bus gocan.BusID, offset time.Duration, id uint32, flags gocan.FrameFlags, data ...byte) gocan.FrameEvent {
	f, err := gocan.NewFrame(id, data, flags)
	if err != nil {
		panic(err)
	}
	return gocan.FrameEvent{Bus: bus, Timestamp: start.Add(offset), Direction: gocan.DirectionReceive, Frame: f}
}

func TestRecording(t *testing.T) {
	var plain, compressed []byte
	t.Run("plain", func(t *testing.T) { plain = testRecording(t, false) })
	t.Run("compressed", func(t *testing.T) { compressed = testRecording(t, true) })
	if !bytes.Equal(plain, compressed) {
		t.Fatal("compression changed the frame record stream")
	}
}

func testRecording(t *testing.T, compression bool) []byte {
	f := newFile(t)
	// Preserve legacy encoding and comments that are absent from the model.
	db, err := dbc.Parse("", "BU_: ECU\nBO_ 291 Status: 8 ECU\n SG_ Value : 0|8@1+ (0.5,-10) [-10|117.5] \"V\" ECU\nCM_ \"Gr\xf6\xdfe\";\n")
	if err != nil {
		t.Fatal(err)
	}
	description := []byte(db.Source())
	second := []byte("BU_: ECU\nBO_ 292 Second: 8 ECU\n SG_ Value : 0|8@1+ (2,0) [0|510] \"V\" ECU\n")
	w, err := mf4.NewWriter(f, start, mf4.Options{Compression: compression, Databases: []mf4.Database{
		{Bus: 1, Name: "bus1.dbc", Data: description},
		{Bus: 2, Name: "bus2.dbc", Data: second},
	}})
	if err != nil {
		t.Fatal(err)
	}
	capture := gocan.NewCapture()
	appendFrame := func(event gocan.FrameEvent) {
		t.Helper()
		if err := capture.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	appendFrame(frame(1, 0, 0x123, 0, 40, 2, 3, 4, 5, 6, 7, 8))
	r, err := recorder.Start(context.Background(), capture, w, gocan.Cursor{}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	initial := capture.End()
	if r.Accepted() != initial || r.Flushed() != initial {
		t.Fatal("initial recorder progress missing")
	}
	if err := capture.Prune(initial); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 64)
	if _, err := f.ReadAt(header, 0); err != nil {
		t.Fatal(err)
	}
	if string(header[:8]) != "UnFinMF " || binary.LittleEndian.Uint16(header[60:]) != 1 {
		t.Fatal("Flush falsely finalised the measurement")
	}
	fd := frame(300, time.Millisecond, 0x1ABCDE, gocan.FrameExtended|gocan.FrameFD|gocan.FrameBitRateSwitch|gocan.FrameErrorStateIndicator, make([]byte, 64)...)
	for i := range fd.Frame.Data {
		fd.Frame.Data[i] = byte(i)
	}
	fd.Direction = gocan.DirectionTransmit
	appendFrame(fd)
	remote, err := gocan.NewRemoteFrame(0x456, 15, false)
	if err != nil {
		t.Fatal(err)
	}
	appendFrame(gocan.FrameEvent{Bus: 1, Timestamp: start.Add(2 * time.Millisecond), Direction: gocan.DirectionTransmit, Frame: remote})
	// Repeated coarse-clock timestamps, including the initial timestamp already
	// flushed by Start, must survive recording and cross the buffer boundary.
	// The final short payload cannot leak FD data from another bus.
	for i := 0; i < 800; i++ {
		appendFrame(frame(1, time.Duration(i/2)*time.Millisecond, 0x123, 0, 42))
	}
	appendFrame(frame(1, 803*time.Millisecond, 0x123, 0, 44))
	appendFrame(frame(2, 803*time.Millisecond, 0x124, 0, 60, 0, 0, 0, 0, 0, 0, 0))
	end := capture.End()
	r.Stop()
	if r.Err() != nil || r.Accepted() != end || r.Flushed() != end {
		t.Fatal("final recorder progress", r.Err())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFrame(fd); err == nil {
		t.Fatal("accepted frame after close")
	}
	if _, err := f.ReadAt(header, 0); err != nil {
		t.Fatal(err)
	}
	if string(header[:8]) != "MDF     " || binary.LittleEndian.Uint16(header[60:]) != 0 {
		t.Fatal("Close did not finalise the measurement")
	}
	checkGroups(t, f, []uint64{802, 1, 1, 1})
	// Follow the MDF header's attachment list and compare the stored source bytes.
	var link [8]byte
	if _, err := f.ReadAt(link[:], 112); err != nil {
		t.Fatal(err)
	}
	attachment := binary.LittleEndian.Uint64(link[:])
	for _, want := range [][]byte{description, second} {
		block := make([]byte, 96+len(want))
		if _, err := f.ReadAt(block, int64(attachment)); err != nil {
			t.Fatal(err)
		}
		if string(block[:4]) != "##AT" || binary.LittleEndian.Uint64(block[88:]) != uint64(len(want)) || !bytes.Equal(block[96:], want) {
			t.Fatal("attachment did not preserve original DBC bytes")
		}
		attachment = binary.LittleEndian.Uint64(block[24:])
	}
	if attachment != 0 {
		t.Fatal("unexpected extra attachment")
	}
	records := readRecords(t, f, compression)
	// Skip the initial classical, FD and remote records. Each pair must retain
	// its shared timestamp without adjustment, including across data chunks.
	for i := 0; i < 800; i++ {
		offset := 89 + 89 + 22 + i*89
		seconds := math.Float64frombits(binary.LittleEndian.Uint64(records[offset+4:]))
		if seconds != float64(i/2)/1000 {
			t.Fatalf("frame %d timestamp changed: %g", i, seconds)
		}
	}
	return records
}

// Follow the MDF data links and inflate with the standard zlib reader. This
// catches broken chunk links, compressed lengths, and logical DL offsets.
func readRecords(t *testing.T, f *os.File, compressed bool) []byte {
	t.Helper()
	file := readImage(t, f)
	dg := file.u64(88)
	addr := file.u64(dg + 40)
	if compressed {
		if string(file[addr:addr+4]) != "##HL" || file[addr+34] != 0 {
			t.Fatal("missing Deflate header list")
		}
		addr = file.u64(addr + 24)
	}
	var records []byte
	chunks := 0
	for addr != 0 {
		if string(file[addr:addr+4]) != "##DL" || file.u64(addr+48) != uint64(len(records)) {
			t.Fatal("bad data-list type or logical offset")
		}
		dataAddr := file.u64(addr + 32)
		payload := file[dataAddr+24 : dataAddr+file.u64(dataAddr+8)]
		if compressed {
			if string(file[dataAddr:dataAddr+4]) != "##DZ" || string(payload[:2]) != "DT" || payload[2] != 0 ||
				binary.LittleEndian.Uint64(payload[16:]) != uint64(len(payload)-24) {
				t.Fatal("bad compressed block parameters")
			}
			reader, err := zlib.NewReader(bytes.NewReader(payload[24:]))
			if err != nil {
				t.Fatal(err)
			}
			inflated, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || uint64(len(inflated)) != binary.LittleEndian.Uint64(payload[8:]) {
				t.Fatal("bad inflated payload", err)
			}
			payload = inflated
		} else if string(file[dataAddr:dataAddr+4]) != "##DT" {
			t.Fatal("missing uncompressed data block")
		}
		records = append(records, payload...)
		addr = file.u64(addr + 24)
		chunks++
	}
	if chunks < 3 || len(records) != 804*89+22 {
		t.Fatalf("incomplete record stream: %d chunks, %d bytes", chunks, len(records))
	}
	return records
}

// Read standard CG counters through the file's links, independent of the
// writer's in-memory state and ordering of metadata blocks.
func checkGroups(t *testing.T, f *os.File, counts []uint64) {
	t.Helper()
	file := readImage(t, f)
	dg := file.u64(88)
	cg := file.u64(dg + 32)
	for _, want := range counts {
		if string(file[cg:cg+4]) != "##CG" || file.u64(cg+80) != want {
			t.Fatalf("bad CG counter, want %d", want)
		}
		cg = file.u64(cg + 24)
	}
	if cg != 0 {
		t.Fatal("unexpected extra group")
	}
}

func TestCaptureEvents(t *testing.T) {
	f := newFile(t)
	w, err := mf4.NewWriter(f, start, mf4.Options{Compression: true})
	if err != nil {
		t.Fatal(err)
	}
	capture := gocan.NewCapture()
	if err := capture.Append(frame(1, 0, 1, 0, 42)); err != nil {
		t.Fatal(err)
	}
	events := []struct {
		event         gocan.Event
		name, details string
	}{
		{gocan.Event{Bus: 300, Kind: gocan.EventControllerState, ControllerState: gocan.ControllerBusOff, ErrorCountsKnown: true, TXErrorCount: 12, RXErrorCount: 34}, "CAN300 controller state", "Bus=300; ControllerState=bus_off; ErrorCountsKnown=true; TXErrorCount=12; RXErrorCount=34"},
		{gocan.Event{Bus: 1, Kind: gocan.EventControllerState, ControllerState: gocan.ControllerActive, ErrorCountsKnown: true}, "CAN1 controller state", "Bus=1; ControllerState=active; ErrorCountsKnown=true; TXErrorCount=0; RXErrorCount=0"},
		{gocan.Event{Bus: 2, Kind: gocan.EventControllerState, ControllerState: gocan.ControllerPassive}, "CAN2 controller state", "Bus=2; ControllerState=passive; ErrorCountsKnown=false"},
		{gocan.Event{Bus: 1, Kind: gocan.EventControllerState, ControllerState: gocan.ControllerWarning}, "CAN1 controller state", "Bus=1; ControllerState=warning; ErrorCountsKnown=false"},
		{gocan.Event{Bus: 1, Kind: gocan.EventErrorFrame}, "CAN1 error observation", ""},
		{gocan.Event{Bus: 1, Kind: gocan.EventReceiveOverrun}, "CAN1 receive overrun", ""},
	}
	for _, entry := range events {
		event := entry.event
		event.Timestamp = start.Add(500 * time.Millisecond)
		if err := capture.AppendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := capture.Append(frame(1, time.Second, 2, 0)); err != nil {
		t.Fatal(err)
	}
	r, err := recorder.Start(context.Background(), capture, w, gocan.Cursor{}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r.Stop()
	if r.Err() != nil || r.Accepted() != capture.End() || r.Flushed() != capture.End() {
		t.Fatal("recorder stopped at an event", r.Err())
	}
	checkGroups(t, f, []uint64{2}) // Event-only buses must not fabricate frame groups.
	file := readImage(t, f)
	addr := file.u64(120)
	for _, want := range events {
		if addr == 0 || string(file[addr:addr+4]) != "##EV" {
			t.Fatal("missing event marker")
		}
		if file.text(file.u64(addr+48)) != want.name {
			t.Fatal("event name or order changed")
		}
		var comment struct {
			Text string `xml:"TX"`
		}
		if want.details == "" {
			if file.u64(addr+56) != 0 {
				t.Fatal("event without details has a redundant comment")
			}
		} else if err := xml.Unmarshal([]byte(file.text(file.u64(addr+56))), &comment); err != nil || comment.Text != want.details {
			t.Fatalf("event detail loss: %q, %v", comment.Text, err)
		}
		if file.u64(addr+16) != 5 || file[addr+64] != 6 || file[addr+65] != 1 || file[addr+66] != 0 ||
			file.u64(addr+80) != 500_000_000 || math.Float64frombits(file.u64(addr+88)) != 1e-9 {
			t.Fatal("event must be a file-level, time-synchronised point marker")
		}
		if file[addr+68] != 0 {
			t.Fatal("capture observation marked as generated during post-processing")
		}
		addr = file.u64(addr + 24)
	}
	if addr != 0 {
		t.Fatal("extra event marker")
	}
	if err := w.WriteEvent(events[0].event); err == nil {
		t.Fatal("accepted event after Close")
	}
}

func TestEventOnlyExport(t *testing.T) {
	f := newFile(t)
	w, err := mf4.NewWriter(f, start, mf4.Options{Compression: true})
	if err != nil {
		t.Fatal(err)
	}
	event := gocan.Event{Bus: 300, Timestamp: start.Add(-time.Nanosecond), Kind: gocan.EventReceiveOverrun}
	if err := w.WriteEvent(event); err == nil {
		t.Fatal("accepted event before start")
	}
	event.Timestamp, event.Kind = start, 0
	if err := w.WriteEvent(event); err == nil {
		t.Fatal("accepted invalid event")
	}
	event.Kind = gocan.EventReceiveOverrun
	if err := w.WriteEvent(event); err != nil {
		t.Fatal("validation poisoned the writer", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	checkGroups(t, f, nil)
	var link [8]byte
	if _, err := f.ReadAt(link[:], 120); err != nil || binary.LittleEndian.Uint64(link[:]) == 0 {
		t.Fatal("event-only export lost the marker", err)
	}
}

type failingFile struct {
	*os.File
	writeErr error
	seekErr  error
	short    bool
}

func (f *failingFile) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.short && len(data) > 0 {
		return f.File.Write(data[:len(data)/2])
	}
	return f.File.Write(data)
}
func (f *failingFile) Seek(offset int64, whence int) (int64, error) {
	if f.seekErr != nil {
		return 0, f.seekErr
	}
	return f.File.Seek(offset, whence)
}

func TestWriterFailures(t *testing.T) {
	for _, stage := range []string{"flush", "short", "finalise", "event"} {
		t.Run(stage, func(t *testing.T) {
			f := &failingFile{File: newFile(t)}
			w, err := mf4.NewWriter(f, start, mf4.Options{Compression: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.WriteFrame(frame(1, 0, 1, 0, 42)); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("storage failure")
			switch stage {
			case "flush":
				f.writeErr = failure
			case "event":
				f.writeErr = failure
				if err := w.WriteEvent(gocan.Event{Bus: 1, Timestamp: start, Kind: gocan.EventErrorFrame}); !errors.Is(err, failure) {
					t.Fatalf("event: %v", err)
				}
			case "short":
				f.short = true
				failure = io.ErrShortWrite
			case "finalise":
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				f.seekErr = failure
			}
			if err := w.Close(); !errors.Is(err, failure) {
				t.Fatalf("close: %v", err)
			}
			f.writeErr, f.seekErr, f.short = nil, nil, false
			if err := w.Close(); !errors.Is(err, failure) {
				t.Fatalf("lost sticky failure: %v", err)
			}
			if err := w.WriteFrame(frame(1, time.Second, 2, 0)); !errors.Is(err, failure) {
				t.Fatalf("accepted after failure: %v", err)
			}
			var marker [8]byte
			if _, err := f.ReadAt(marker[:], 0); err != nil {
				t.Fatal(err)
			}
			if string(marker[:]) != "UnFinMF " {
				t.Fatal("failed output marked finalised")
			}
		})
	}
	f := newFile(t)
	w, err := mf4.NewWriter(f, start, mf4.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFrame(frame(1, -time.Second, 1, 0)); err == nil {
		t.Fatal("accepted timestamp before start")
	}
	if err := w.WriteFrame(frame(1, 0, 1, 0)); err != nil {
		t.Fatal("validation poisoned writer", err)
	}
	if err := w.WriteFrame(frame(1, 0, 2, 0)); err != nil {
		t.Fatal("duplicate time rejected", err)
	}
	if err := w.WriteFrame(frame(1, time.Hour, 2, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFrame(frame(1, time.Second, 2, 0)); err == nil {
		t.Fatal("regressing time accepted")
	}
	// Distinct nanosecond times can encode to the same float64 in long runs.
	late := frame(1, 365*24*time.Hour, 3, 0)
	if err := w.WriteFrame(late); err != nil {
		t.Fatal(err)
	}
	late.Timestamp = late.Timestamp.Add(time.Nanosecond)
	if err := w.WriteFrame(late); err != nil {
		t.Fatal("indistinguishable encoded time rejected", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := mf4.NewWriter(f, start, mf4.Options{}); err == nil {
		t.Fatal("overwrote existing output")
	}
}
