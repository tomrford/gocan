package mf4_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/dbc"
	"github.com/tomrford/gocan/mf4"
	"github.com/tomrford/gocan/recorder"
)

var start = time.Date(2026, 9, 6, 10, 0, 0, 123456789, time.UTC)

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
	f := newFile(t)
	// Preserve legacy encoding and comments that are absent from the model.
	db, err := dbc.Parse("", "BU_: ECU\nBO_ 291 Status: 8 ECU\n SG_ Value : 0|8@1+ (0.5,-10) [-10|117.5] \"V\" ECU\nCM_ \"Gr\xf6\xdfe\";\n")
	if err != nil {
		t.Fatal(err)
	}
	description := []byte(db.Source())
	second := []byte("BU_: ECU\nBO_ 292 Second: 8 ECU\n SG_ Value : 0|8@1+ (2,0) [0|510] \"V\" ECU\n")
	w, err := mf4.NewWriter(f, start, []mf4.Database{
		{Bus: 1, Name: "bus1.dbc", Data: description},
		{Bus: 2, Name: "bus2.dbc", Data: second},
	})
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
	// Many small frames cross the writer's buffer boundary. The final short
	// payload cannot leak FD data from another bus.
	for i := 0; i < 800; i++ {
		appendFrame(frame(1, time.Duration(i+3)*time.Millisecond, 0x123, 0, 42))
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
}

// Read standard CG counters through the file's links, independent of the
// writer's in-memory state and ordering of metadata blocks.
func checkGroups(t *testing.T, f *os.File, counts []uint64) {
	t.Helper()
	read := func(offset int64, size int) []byte {
		t.Helper()
		data := make([]byte, size)
		if _, err := f.ReadAt(data, offset); err != nil {
			t.Fatal(err)
		}
		return data
	}
	dg := binary.LittleEndian.Uint64(read(88, 8))
	cg := binary.LittleEndian.Uint64(read(int64(dg)+32, 8))
	for _, want := range counts {
		block := read(int64(cg), 104)
		if string(block[:4]) != "##CG" || binary.LittleEndian.Uint64(block[80:]) != want {
			t.Fatalf("bad CG counter, want %d", want)
		}
		cg = binary.LittleEndian.Uint64(block[24:])
	}
	if cg != 0 {
		t.Fatal("unexpected extra group")
	}
}

func TestUnsupportedEventStopsAtAcceptedPrefix(t *testing.T) {
	f := newFile(t)
	w, err := mf4.NewWriter(f, start, nil)
	if err != nil {
		t.Fatal(err)
	}
	capture := gocan.NewCapture()
	if err := capture.Append(frame(1, 0, 1, 0, 42)); err != nil {
		t.Fatal(err)
	}
	prefix := capture.End()
	if err := capture.AppendEvent(gocan.Event{Bus: 1, Timestamp: start, Kind: gocan.EventReceiveOverrun}); err != nil {
		t.Fatal(err)
	}
	if err := capture.Append(frame(1, time.Second, 2, 0)); err != nil {
		t.Fatal(err)
	}
	r, err := recorder.Start(context.Background(), capture, w, gocan.Cursor{}, time.Hour)
	var recordErr *gocan.RecordWriteError
	if !errors.As(err, &recordErr) {
		t.Fatalf("missing record failure: %v", err)
	}
	if r.Accepted() != prefix || r.Flushed() != prefix {
		t.Fatal("recorder hid unsupported event")
	}
	checkGroups(t, f, []uint64{1})
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
	for _, stage := range []string{"flush", "short", "finalise"} {
		t.Run(stage, func(t *testing.T) {
			f := &failingFile{File: newFile(t)}
			w, err := mf4.NewWriter(f, start, nil)
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
	w, err := mf4.NewWriter(f, start, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFrame(frame(1, -time.Second, 1, 0)); err == nil {
		t.Fatal("accepted timestamp before start")
	}
	if err := w.WriteFrame(frame(1, 0, 1, 0)); err != nil {
		t.Fatal("validation poisoned writer", err)
	}
	if err := w.WriteFrame(frame(1, 0, 2, 0)); err == nil {
		t.Fatal("duplicate time accepted")
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
	if err := w.WriteFrame(late); err == nil {
		t.Fatal("indistinguishable encoded time accepted")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := mf4.NewWriter(f, start, nil); err == nil {
		t.Fatal("overwrote existing output")
	}
}
