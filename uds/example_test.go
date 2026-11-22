package uds_test

import (
	"context"
	"fmt"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/isotp"
	"github.com/tomrford/gocan/uds"
)

func ExampleClient() {
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

	testerLink, err := isotp.New(testerBus, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
	if err != nil {
		panic(err)
	}
	ecuLink, err := isotp.New(ecuBus, isotp.Config{TransmitID: 0x7e8, ReceiveID: 0x7e0})
	if err != nil {
		panic(err)
	}
	client, err := uds.New(testerLink, uds.Config{})
	if err != nil {
		panic(err)
	}

	served := make(chan error, 1)
	go func() {
		if _, err := ecuLink.Receive(ctx); err != nil {
			served <- err
			return
		}
		served <- ecuLink.Send(ctx, []byte{0x62, 0xf1, 0x90, 'V', 'I', 'N'})
	}()

	vin, err := client.ReadDataByIdentifier(ctx, 0xf190)
	if err != nil {
		panic(err)
	}
	if err := <-served; err != nil {
		panic(err)
	}
	fmt.Println(string(vin))
	// Output:
	// VIN
}
