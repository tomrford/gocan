package cyclic_test

import (
	"context"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/cyclic"
	"github.com/tomrford/gocan/drivers/virtual"
)

func ExampleStart() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	capture := gocan.NewCapture()
	var network virtual.Network
	bus, err := network.Open(ctx, capture, virtual.Config{ID: 1, Name: "can"})
	if err != nil {
		panic(err)
	}
	defer bus.Close()

	frame, err := gocan.NewFrame(0x123, []byte{0x01, 0x02}, 0)
	if err != nil {
		panic(err)
	}
	cursor := capture.End()
	task, err := cyclic.Start(ctx, bus, frame, cyclic.Config{Period: 50 * time.Millisecond})
	if err != nil {
		panic(err)
	}
	defer task.Close()

	sent, _, err := capture.Next(ctx, gocan.FrameKey{
		Bus:       bus.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionTransmit,
	}, cursor)
	if err != nil {
		panic(err)
	}
	fmt.Println(sent.Frame)
	// Output:
	// 123#0102
}
