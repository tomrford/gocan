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
