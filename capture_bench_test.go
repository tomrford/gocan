package gocan

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// These benchmarks verify the claim behind the single append mutex: a
// saturated classical bus is ~9k frames/s and even an 8-bus CAN FD rig stays
// under ~100k appends/s, so the serialised path must deliver comfortable
// multiples of that with readers attached. Clear runs periodically so memory
// stays bounded; its cost amortises to noise.

const benchClearInterval = 1 << 20

var benchTimestamp = time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

func benchEvent(bus BusID, id uint32, size int) FrameEvent {
	var flags FrameFlags
	if size > 8 {
		flags = FrameFD
	}
	frame, err := NewFrame(id, make([]byte, size), flags)
	if err != nil {
		panic(err)
	}
	return FrameEvent{
		Bus:       bus,
		Timestamp: benchTimestamp,
		Direction: DirectionReceive,
		Frame:     frame,
	}
}

func BenchmarkAppend(b *testing.B) {
	for _, size := range []int{8, 64} {
		b.Run(fmt.Sprintf("payload%d", size), func(b *testing.B) {
			capture := NewCapture()
			event := benchEvent(1, 0x100, size)
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				if err := capture.Append(event); err != nil {
					b.Fatal(err)
				}
				if i%benchClearInterval == benchClearInterval-1 {
					capture.Clear()
				}
			}
		})
	}
}

func BenchmarkAppendParallel(b *testing.B) {
	capture := NewCapture()
	var buses atomic.Uint32
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		event := benchEvent(BusID(buses.Add(1)), 0x100, 8)
		count := 0
		for pb.Next() {
			if err := capture.Append(event); err != nil {
				b.Error(err)
				return
			}
			if count++; count%benchClearInterval == 0 {
				capture.Clear()
			}
		}
	})
}

// BenchmarkAppendUnderLoad measures the append path while consumers behave as
// protocol layers will: one reader polls the hot series frontier with
// SeriesSince, and one waits on a rare key through Next, woken every 1024th
// append.
func BenchmarkAppendUnderLoad(b *testing.B) {
	capture := NewCapture()
	hot := benchEvent(1, 0x100, 8)
	rare := benchEvent(1, 0x200, 8)

	ctx, cancel := context.WithCancel(context.Background())
	consumers := make(chan struct{}, 2)
	go func() {
		defer func() { consumers <- struct{}{} }()
		var cursor Cursor
		var err error
		for ctx.Err() == nil {
			_, cursor, err = capture.SeriesSince(FrameKey{Bus: 1, ID: 0x100, Direction: DirectionReceive}, cursor)
			if err != nil {
				b.Errorf("SeriesSince: %v", err)
				return
			}
		}
	}()
	go func() {
		defer func() { consumers <- struct{}{} }()
		var cursor Cursor
		for {
			_, next, err := capture.Next(ctx, FrameKey{Bus: 1, ID: 0x200, Direction: DirectionReceive}, cursor)
			if err != nil {
				return
			}
			cursor = next
		}
	}()

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		event := hot
		if i%1024 == 1023 {
			event = rare
		}
		if err := capture.Append(event); err != nil {
			b.Fatal(err)
		}
		if i%benchClearInterval == benchClearInterval-1 {
			capture.Clear()
		}
	}
	cancel()
	<-consumers
	<-consumers
}
