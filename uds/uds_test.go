package uds_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/isotp"
	"github.com/tomrford/gocan/uds"
)

func TestClientExchangeLifecycle(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	testerBus, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		t.Fatalf("Open tester: %v", err)
	}
	t.Cleanup(func() { _ = testerBus.Close() })
	ecuBus, err := network.Open(context.Background(), capture, virtual.Config{ID: 2, Name: "ECU"})
	if err != nil {
		t.Fatalf("Open ECU: %v", err)
	}
	t.Cleanup(func() { _ = ecuBus.Close() })

	testerLink, err := isotp.New(testerBus, capture, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
	if err != nil {
		t.Fatalf("New tester link: %v", err)
	}
	ecuLink, err := isotp.New(ecuBus, capture, isotp.Config{TransmitID: 0x7e8, ReceiveID: 0x7e0})
	if err != nil {
		t.Fatalf("New ECU link: %v", err)
	}
	client, err := uds.New(testerLink, uds.Config{
		P2Timeout:     200 * time.Millisecond,
		P2StarTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	responseData := bytes.Repeat([]byte{0x5a}, 80)
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- runServerLifecycle(ctx, ecuLink, responseData)
	}()

	response, err := client.Do(ctx, uds.Request{Service: 0x22, Data: []byte{0xf1, 0x90}})
	if err != nil {
		t.Fatalf("Read DID: %v", err)
	}
	if response.Service != 0x22 || !bytes.Equal(response.Data, responseData) {
		t.Fatalf("Read DID response = service %#x data %x", response.Service, response.Data)
	}

	_, err = client.Do(ctx, uds.Request{Service: 0x31, Data: []byte{1, 0x12, 0x34}})
	var negative *uds.NegativeResponseError
	if !errors.As(err, &negative) {
		t.Fatalf("Routine Control = %v, want NegativeResponseError", err)
	}
	if negative.Service != 0x31 || negative.Code != 0x22 {
		t.Fatalf("negative response = service %#x code %#x", negative.Service, negative.Code)
	}

	if err := client.Send(ctx, uds.Request{Service: 0x3e, Data: []byte{0x80}}); err != nil {
		t.Fatalf("Send Tester Present: %v", err)
	}

	_, err = client.Do(ctx, uds.Request{Service: 0x10, Data: []byte{3}})
	if !errors.Is(err, uds.ErrP2StarTimeout) {
		t.Fatalf("pending Session Control = %v, want ErrP2StarTimeout", err)
	}

	response, err = client.Do(ctx, uds.Request{Service: 0x11, Data: []byte{1}})
	if err != nil {
		t.Fatalf("ECU Reset after timeout: %v", err)
	}
	if response.Service != 0x11 || !bytes.Equal(response.Data, []byte{1}) {
		t.Fatalf("ECU Reset response = service %#x data %x", response.Service, response.Data)
	}

	if err := <-serverResult; err != nil {
		t.Fatalf("ECU: %v", err)
	}
}

func runServerLifecycle(ctx context.Context, link *isotp.Link, responseData []byte) error {
	if err := receiveRequest(ctx, link, []byte{0x22, 0xf1, 0x90}); err != nil {
		return err
	}
	if err := link.Send(ctx, []byte{0x7f, 0x22, 0x78}); err != nil {
		return fmt.Errorf("send ResponsePending: %w", err)
	}
	positive := append([]byte{0x62}, responseData...)
	if err := link.Send(ctx, positive); err != nil {
		return fmt.Errorf("send Read DID response: %w", err)
	}

	if err := receiveRequest(ctx, link, []byte{0x31, 1, 0x12, 0x34}); err != nil {
		return err
	}
	if err := link.Send(ctx, []byte{0x7f, 0x31, 0x22}); err != nil {
		return fmt.Errorf("send negative response: %w", err)
	}

	if err := receiveRequest(ctx, link, []byte{0x3e, 0x80}); err != nil {
		return err
	}
	if err := receiveRequest(ctx, link, []byte{0x10, 3}); err != nil {
		return err
	}
	if err := link.Send(ctx, []byte{0x7f, 0x10, 0x78}); err != nil {
		return fmt.Errorf("send final ResponsePending: %w", err)
	}

	if err := receiveRequest(ctx, link, []byte{0x11, 1}); err != nil {
		return err
	}
	if err := link.Send(ctx, []byte{0x51, 1}); err != nil {
		return fmt.Errorf("send ECU Reset response: %w", err)
	}
	return nil
}

func receiveRequest(ctx context.Context, link *isotp.Link, want []byte) error {
	request, err := link.Receive(ctx)
	if err != nil {
		return fmt.Errorf("receive %x: %w", want, err)
	}
	if !bytes.Equal(request, want) {
		return fmt.Errorf("request = %x, want %x", request, want)
	}
	return nil
}
