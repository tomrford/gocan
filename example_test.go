package gocan_test

import (
	"context"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
)

func Example() {
	ctx := context.Background()
	capture := gocan.NewCapture()
	var network virtual.Network

	sender, err := network.Open(ctx, capture, virtual.Config{ID: 1, Name: "sender"})
	if err != nil {
		panic(err)
	}
	defer sender.Close()
	receiver, err := network.Open(ctx, capture, virtual.Config{ID: 2, Name: "receiver"})
	if err != nil {
		panic(err)
	}
	defer receiver.Close()

	frame, err := gocan.NewFrame(0x123, []byte{1, 2, 3, 4}, 0)
	if err != nil {
		panic(err)
	}
	cursor := capture.End()
	if err := sender.Send(ctx, frame); err != nil {
		panic(err)
	}

	wait, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	received, _, err := capture.Next(wait, gocan.FrameKey{
		Bus:       receiver.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionReceive,
	}, cursor)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%x\n", received.Frame.Data[:received.Frame.DataLength()])
	// Output:
	// 01020304
}

func ExampleCapture_Frames() {
	capture := gocan.NewCapture()
	at := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	for i, id := range []uint32{0x100, 0x200} {
		frame, err := gocan.NewFrame(id, []byte{byte(i + 1)}, 0)
		if err != nil {
			panic(err)
		}
		if err := capture.Append(gocan.FrameEvent{
			Bus:       1,
			Timestamp: at.Add(time.Duration(i) * time.Millisecond),
			Direction: gocan.DirectionReceive,
			Frame:     frame,
		}); err != nil {
			panic(err)
		}
	}

	frames, cursor, err := capture.Frames(gocan.Range{})
	if err != nil {
		panic(err)
	}
	for event := range frames {
		fmt.Printf("%#x\n", event.Frame.ID)
	}

	// Read only records appended after cursor.
	extra, err := gocan.NewFrame(0x300, []byte{3}, 0)
	if err != nil {
		panic(err)
	}
	if err := capture.Append(gocan.FrameEvent{
		Bus:       1,
		Timestamp: at.Add(2 * time.Millisecond),
		Direction: gocan.DirectionReceive,
		Frame:     extra,
	}); err != nil {
		panic(err)
	}
	later, _, err := capture.Frames(gocan.Since(cursor))
	if err != nil {
		panic(err)
	}
	for event := range later {
		fmt.Printf("later %#x\n", event.Frame.ID)
	}
	// Output:
	// 0x100
	// 0x200
	// later 0x300
}
