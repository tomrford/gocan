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

	testerLink, err := isotp.New(testerBus, isotp.Config{
		TransmitID: 0x7e0,
		ReceiveID:  0x7e8,
		// Keep the segmented response in progress beyond P2* after its First
		// Frame arrives.
		AdvertisedSeparationTime: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New tester link: %v", err)
	}
	ecuLink, err := isotp.New(ecuBus, isotp.Config{TransmitID: 0x7e8, ReceiveID: 0x7e0})
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
	functionalPath, err := isotp.NewFunctional(testerBus, isotp.FunctionalConfig{TransmitID: 0x7df})
	if err != nil {
		t.Fatalf("New functional path: %v", err)
	}
	functional, err := uds.NewFunctional(functionalPath, uds.FunctionalConfig{})
	if err != nil {
		t.Fatalf("New functional client: %v", err)
	}
	functionalServer, err := isotp.New(ecuBus, isotp.Config{TransmitID: 0x7e8, ReceiveID: 0x7df})
	if err != nil {
		t.Fatalf("New functional server: %v", err)
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

	// A pending that echoes a stale service ID still restarts the wait.
	response, err = client.Do(ctx, uds.Request{Service: 0x36, Data: []byte{1}})
	if err != nil {
		t.Fatalf("Transfer Data with mislabeled pending: %v", err)
	}
	if response.Service != 0x36 || !bytes.Equal(response.Data, []byte{1}) {
		t.Fatalf("Transfer Data response = service %#x data %x", response.Service, response.Data)
	}

	if err := <-serverResult; err != nil {
		t.Fatalf("ECU: %v", err)
	}

	broadcasts := []struct {
		send func() error
		want []byte
	}{
		{
			send: func() error {
				return functional.SendCommunicationControl(ctx, uds.CommunicationDisableRxAndTx, uds.CommunicationTypeNormalAndNetworkManagement)
			},
			want: []byte{0x28, 0x83, 0x03},
		},
		{
			send: func() error { return functional.SendControlDTCSetting(ctx, uds.DTCSettingOff, nil) },
			want: []byte{0x85, 0x82},
		},
		{
			send: func() error { return functional.SendTesterPresent(ctx) },
			want: []byte{0x3e, 0x80},
		},
	}
	for _, broadcast := range broadcasts {
		if err := broadcast.send(); err != nil {
			t.Fatalf("send functional request %x: %v", broadcast.want, err)
		}
		if err := receiveRequest(ctx, functionalServer, broadcast.want); err != nil {
			t.Fatal(err)
		}
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

	if err := receiveRequest(ctx, link, []byte{0x36, 1}); err != nil {
		return err
	}
	if err := link.Send(ctx, []byte{0x7f, 0x31, 0x78}); err != nil {
		return fmt.Errorf("send mislabeled ResponsePending: %w", err)
	}
	if err := link.Send(ctx, []byte{0x76, 1}); err != nil {
		return fmt.Errorf("send Transfer Data response: %w", err)
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

func TestClientP3ClientSpacesRequests(t *testing.T) {
	const p3 = 40 * time.Millisecond
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

	testerLink, err := isotp.New(testerBus, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
	if err != nil {
		t.Fatalf("New tester link: %v", err)
	}
	ecuLink, err := isotp.New(ecuBus, isotp.Config{TransmitID: 0x7e8, ReceiveID: 0x7e0})
	if err != nil {
		t.Fatalf("New ECU link: %v", err)
	}
	client, err := uds.New(testerLink, uds.Config{P3Client: p3})
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	functionalPath, err := isotp.NewFunctional(testerBus, isotp.FunctionalConfig{TransmitID: 0x7df})
	if err != nil {
		t.Fatalf("New functional path: %v", err)
	}
	functional, err := uds.NewFunctional(functionalPath, uds.FunctionalConfig{P3Client: p3})
	if err != nil {
		t.Fatalf("New functional client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverResult := make(chan error, 1)
	go func() {
		for range 2 {
			if err := receiveRequest(ctx, ecuLink, []byte{0x22, 0xf1, 0x90}); err != nil {
				serverResult <- err
				return
			}
			if err := ecuLink.Send(ctx, []byte{0x62, 0xf1, 0x90, 0x00}); err != nil {
				serverResult <- err
				return
			}
		}
		serverResult <- nil
	}()

	if err := functional.SendTesterPresent(ctx); err != nil {
		t.Fatalf("functional Tester Present: %v", err)
	}
	if _, err := client.ReadDataByIdentifier(ctx, 0xf190); err != nil {
		t.Fatalf("first Read DID: %v", err)
	}
	if err := functional.SendTesterPresent(ctx); err != nil {
		t.Fatalf("second functional Tester Present: %v", err)
	}
	if _, err := client.ReadDataByIdentifier(ctx, 0xf190); err != nil {
		t.Fatalf("second Read DID: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("ECU: %v", err)
	}

	var requests []gocan.FrameEvent
	for _, frame := range capture.FramesBetween(gocan.Cursor{}, capture.End()) {
		if frame.Bus != testerBus.ID() || frame.Direction != gocan.DirectionTransmit {
			continue
		}
		if frame.Frame.ID != 0x7df && frame.Frame.ID != 0x7e0 {
			continue
		}
		requests = append(requests, frame)
	}
	if len(requests) != 4 {
		t.Fatalf("tester requests = %d, want 4", len(requests))
	}
	for index := 1; index < len(requests); index++ {
		gap := requests[index].Timestamp.Sub(requests[index-1].Timestamp)
		if gap < p3 {
			t.Fatalf("request %d after %d gap %s, want at least %s", index, index-1, gap, p3)
		}
	}
}
