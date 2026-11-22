package isotp_test

import (
	"context"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/isotp"
)

func ExampleLink() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	capture := gocan.NewCapture()
	var network virtual.Network
	testerBus, err := network.Open(ctx, capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		panic(err)
	}
	defer testerBus.Close()
	ecuBus, err := network.Open(ctx, capture, virtual.Config{ID: 2, Name: "ecu"})
	if err != nil {
		panic(err)
	}
	defer ecuBus.Close()

	tester, err := isotp.New(testerBus, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
	if err != nil {
		panic(err)
	}
	ecu, err := isotp.New(ecuBus, isotp.Config{TransmitID: 0x7e8, ReceiveID: 0x7e0})
	if err != nil {
		panic(err)
	}

	sent := make(chan error, 1)
	go func() {
		sent <- tester.Send(ctx, []byte("hello iso-tp"))
	}()

	payload, err := ecu.Receive(ctx)
	if err != nil {
		panic(err)
	}
	if err := <-sent; err != nil {
		panic(err)
	}
	fmt.Println(string(payload))
	// Output:
	// hello iso-tp
}
