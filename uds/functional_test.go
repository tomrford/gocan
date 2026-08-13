package uds_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/isotp"
	"github.com/tomrford/gocan/uds"
)

func TestFunctionalBroadcastLifecycle(t *testing.T) {
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

	path, err := isotp.NewFunctional(testerBus, isotp.FunctionalConfig{TransmitID: 0x7df})
	if err != nil {
		t.Fatalf("NewFunctional path: %v", err)
	}
	functional, err := uds.NewFunctional(path)
	if err != nil {
		t.Fatalf("NewFunctional client: %v", err)
	}
	server, err := isotp.New(ecuBus, capture, isotp.Config{TransmitID: 0x7e8, ReceiveID: 0x7df})
	if err != nil {
		t.Fatalf("New server link: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

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
		{
			send: func() error {
				return functional.Send(ctx, uds.Request{Service: uds.ServiceECUReset, Data: []byte{0x81}})
			},
			want: []byte{0x11, 0x81},
		},
	}
	for _, broadcast := range broadcasts {
		if err := broadcast.send(); err != nil {
			t.Fatalf("send %x: %v", broadcast.want, err)
		}
		if err := receiveRequest(ctx, server, broadcast.want); err != nil {
			t.Fatal(err)
		}
	}

	err = functional.SendCommunicationControl(ctx, 0x81, uds.CommunicationTypeNormal)
	if err == nil || !strings.Contains(err.Error(), "suppressPositiveResponse") {
		t.Fatalf("suppressed control type error = %v", err)
	}
}
