package mf4_test

import (
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/mf4"
)

func TestStartFromFirstRecord(t *testing.T) {
	for _, first := range []string{"frame", "event", "epoch"} {
		t.Run(first, func(t *testing.T) {
			f := newFile(t)
			w, err := mf4.NewWriter(f, mf4.Options{})
			if err != nil {
				t.Fatal(err)
			}
			// Neither invalid records nor an early Flush may establish the start.
			initial := readImage(t, f).u64(136)
			for _, timestamp := range []time.Time{time.Time{}, time.Unix(-1, 0), time.Unix(0, math.MaxInt64).Add(time.Nanosecond)} {
				invalid := frame(1, 0, 1, 0)
				invalid.Timestamp = timestamp
				if err := w.WriteFrame(invalid); err == nil {
					t.Fatal("accepted invalid frame timestamp", timestamp)
				}
				if err := w.WriteEvent(gocan.Event{Bus: 1, Timestamp: timestamp, Kind: gocan.EventErrorFrame}); err == nil {
					t.Fatal("accepted invalid event timestamp", timestamp)
				}
			}
			invalid := frame(0, -time.Hour, 1, 0)
			if err := w.WriteFrame(invalid); err == nil {
				t.Fatal("accepted invalid bus")
			}
			if err := w.WriteEvent(gocan.Event{Bus: 1, Timestamp: start.Add(-time.Hour)}); err == nil {
				t.Fatal("accepted invalid event kind")
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			if readImage(t, f).u64(136) != initial {
				t.Fatal("rejected record or Flush changed the start")
			}
			origin := start
			if first == "epoch" {
				origin = time.Unix(0, 0)
			}
			data := frame(1, 0, 0x123, 0, 42)
			data.Timestamp = origin
			event := gocan.Event{Bus: 2, Timestamp: origin, Kind: gocan.EventErrorFrame}
			wantFrame, wantEvent := 0.0, uint64(250_000_000)
			if first == "event" {
				if err := w.WriteEvent(event); err != nil {
					t.Fatal(err)
				}
				data.Timestamp = origin.Add(250 * time.Millisecond)
				wantFrame, wantEvent = 0.25, 0
			}
			if err := w.WriteFrame(data); err != nil {
				t.Fatal(err)
			}
			if first != "event" {
				event.Timestamp = origin.Add(250 * time.Millisecond)
				if err := w.WriteEvent(event); err != nil {
					t.Fatal(err)
				}
			}
			if readImage(t, f).u64(136) != uint64(origin.UnixNano()) {
				t.Fatal("header did not adopt the first record's timestamp")
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			file := readImage(t, f)
			dt := file.u64(file.u64(file.u64(88)+40) + 32)
			if file.u64(136) != uint64(origin.UnixNano()) ||
				math.Float64frombits(file.u64(dt+24+4)) != wantFrame ||
				file.u64(file.u64(120)+80) != wantEvent {
				t.Fatal("header, frame and event timestamps disagree")
			}
		})
	}
}

func TestEmptyStart(t *testing.T) {
	f := newFile(t)
	before := time.Now().UnixNano()
	w, err := mf4.NewWriter(f, mf4.Options{})
	after := time.Now().UnixNano()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	file := readImage(t, f)
	created := int64(file.u64(136))
	if created < before || created > after || string(file[:8]) != "MDF     " {
		t.Fatal("empty export did not finalise with creation time")
	}
	if err := w.WriteFrame(frame(1, 0, 1, 0)); err == nil {
		t.Fatal("accepted first frame after Close")
	}
	if err := w.WriteEvent(gocan.Event{Bus: 1, Timestamp: start, Kind: gocan.EventErrorFrame}); err == nil {
		t.Fatal("accepted first event after Close")
	}
	if readImage(t, f).u64(136) != uint64(created) {
		t.Fatal("closed writer changed its start")
	}
}

func TestStartPatchFailure(t *testing.T) {
	for _, stage := range []string{"seek", "write", "short"} {
		t.Run(stage, func(t *testing.T) {
			f := &failingFile{File: newFile(t)}
			w, err := mf4.NewWriter(f, mf4.Options{})
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("storage failure")
			switch stage {
			case "seek":
				f.seekErr = failure
			case "write":
				f.writeErr = failure
			case "short":
				f.short = true
				failure = io.ErrShortWrite
			}
			if err := w.WriteFrame(frame(1, 0, 1, 0)); !errors.Is(err, failure) {
				t.Fatalf("first frame: %v", err)
			}
			f.seekErr, f.writeErr, f.short = nil, nil, false
			if err := w.WriteEvent(gocan.Event{Bus: 1, Timestamp: start, Kind: gocan.EventErrorFrame}); !errors.Is(err, failure) {
				t.Fatalf("lost sticky failure: %v", err)
			}
			if err := w.Close(); !errors.Is(err, failure) {
				t.Fatalf("close: %v", err)
			}
			file := readImage(t, f.File)
			if string(file[:8]) != "UnFinMF " || file.u64(120) != 0 || file.u64(file.u64(88)+32) != 0 {
				t.Fatal("failed start published records or finalised output")
			}
		})
	}
}
