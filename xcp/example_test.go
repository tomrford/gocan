package xcp_test

import (
	"context"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/xcp"
)

func ExampleClient() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	capture := gocan.NewCapture()
	var network virtual.Network
	masterBus, err := network.Open(ctx, capture, virtual.Config{ID: 1, Name: "master"})
	if err != nil {
		panic(err)
	}
	defer masterBus.Close()
	ecuBus, err := network.Open(ctx, capture, virtual.Config{ID: 2, Name: "ecu"})
	if err != nil {
		panic(err)
	}
	defer ecuBus.Close()

	client, err := xcp.New(masterBus, xcp.Config{TransmitID: 0x554, ReceiveID: 0x555})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	cursor := capture.End()
	served := make(chan error, 1)
	go func() {
		_, _, err := capture.Next(ctx, gocan.FrameKey{
			Bus:       ecuBus.ID(),
			ID:        0x554,
			Direction: gocan.DirectionReceive,
		}, cursor)
		if err != nil {
			served <- err
			return
		}
		reply, err := gocan.NewFrame(0x555, []byte{0xff, 0x05, 0x00, 8, 8, 0, 1, 1}, 0)
		if err != nil {
			served <- err
			return
		}
		served <- ecuBus.Send(ctx, reply)
	}()

	caps, err := client.Connect(ctx)
	if err != nil {
		panic(err)
	}
	if err := <-served; err != nil {
		panic(err)
	}
	fmt.Printf("MaxCTO=%d MaxDTO=%d\n", caps.MaxCTO, caps.MaxDTO)
	// Output:
	// MaxCTO=8 MaxDTO=8
}
