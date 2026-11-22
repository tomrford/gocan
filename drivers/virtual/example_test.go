package virtual_test

import (
	"context"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
)

func ExampleNetwork() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	capture := gocan.NewCapture()
	var network virtual.Network
	bus, err := network.Open(ctx, capture, virtual.Config{
		ID:                 1,
		Name:               "loopback",
		ReceiveOwnMessages: true,
	})
	if err != nil {
		panic(err)
	}
	defer bus.Close()

	frame, err := gocan.NewFrame(0x100, []byte{0xde, 0xad}, 0)
	if err != nil {
		panic(err)
	}
	cursor := capture.End()
	if err := bus.Send(ctx, frame); err != nil {
		panic(err)
	}
	got, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       bus.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionReceive,
	}, cursor)
	if err != nil {
		panic(err)
	}
	fmt.Println(got.Frame)
	// Output:
	// 100#DEAD
}
