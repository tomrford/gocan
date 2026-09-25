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

	testerTransport := &flowControlBus{Bus: testerBus}
	testerLink, err := isotp.New(testerTransport, isotp.Config{
		TransmitID: 0x7e0,
		ReceiveID:  0x7e8,
		// Keep the segmented response in progress beyond P2* after its First
		// Frame arrives.
		AdvertisedSeparationTime: 30 * time.Millisecond,
		ConsecutiveFrameTimeout:  200 * time.Millisecond,
		TransmitRetryTimeout:     5 * time.Millisecond,
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
	functional, err := uds.NewFunctional(functionalPath)
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
	steps := []struct {
		name      string
		request   []byte
		responses [][]byte
		mode      string // Empty for Do, "send" for Send, "wait" for DoSuppressed.
		want      []byte
		kind      error
		nrc       uds.ResponseCode
		timeout   time.Duration
		cancel    bool
		partial   bool
		blockFlow bool
	}{
		{name: "segmented DID", request: []byte{0x22, 0xf1, 0x90}, responses: [][]byte{{0x7f, 0x22, 0x78}, append([]byte{0x62}, responseData...)}, want: responseData},
		{name: "rejected routine", request: []byte{0x31, 1, 0x12, 0x34}, responses: [][]byte{{0x7f, 0x31, 0x22}}, nrc: 0x22},
		{name: "send-only tester", request: []byte{0x3e, 0x80}, mode: "send", timeout: 100 * time.Millisecond},
		{name: "pending session timeout", request: []byte{0x10, 3}, responses: [][]byte{{0x7f, 0x10, 0x78}}, kind: uds.ErrP2StarTimeout},
		{name: "reset after timeout", request: []byte{0x11, 1}, responses: [][]byte{{0x51, 1}}, want: []byte{1}},
		{name: "mislabeled pending", request: []byte{0x36, 1}, responses: [][]byte{{0x7f, 0x31, 0x78}, {0x76, 1}}, want: []byte{1}},
		{name: "suppressed silence", request: []byte{0x11, 0x81}, mode: "wait"},
		{name: "suppressed positive", request: []byte{0x11, 0x81}, mode: "wait", responses: [][]byte{{0x51, 1}}, want: []byte{1}},
		{name: "suppressed empty positive", request: []byte{0x11, 0x81}, mode: "wait", responses: [][]byte{{0x51}}, want: []byte{}},
		{name: "suppressed malformed response", request: []byte{0x11, 0x81}, mode: "wait", responses: [][]byte{{0x7f, 0x11}}, kind: uds.ErrInvalidResponse},
		{name: "unsuppressed silence", request: []byte{0x11, 1}, kind: uds.ErrP2Timeout},
		{name: "deadline before P2", request: []byte{0x3e, 0x80}, mode: "wait", timeout: 100 * time.Millisecond, kind: context.DeadlineExceeded},
		{name: "suppressed rejection", request: []byte{0x11, 0x81}, mode: "wait", responses: [][]byte{{0x7f, 0x11, 0x22}}, nrc: 0x22},
		{name: "suppressed pending completion", request: []byte{0x31, 0x81, 0x12, 0x34}, mode: "wait", responses: [][]byte{{0x7f, 0x31, 0x78}, {0x71, 1, 0x12, 0x34}}, want: []byte{1, 0x12, 0x34}},
		{name: "suppressed pending timeout", request: []byte{0x10, 0x83}, mode: "wait", responses: [][]byte{{0x7f, 0x10, 0x78}}, kind: uds.ErrP2StarTimeout},
		{name: "cancel suppressed wait", request: []byte{0x11, 0x81}, mode: "wait", cancel: true, kind: context.Canceled},
		{name: "reset after cancellation", request: []byte{0x11, 1}, responses: [][]byte{{0x51, 1}}, want: []byte{1}},
		{name: "suppressed Flow Control timeout", request: []byte{0x11, 0x81}, mode: "wait", partial: true, blockFlow: true, kind: gocan.ErrTransmitQueueFull},
		{name: "unsuppressed Flow Control timeout", request: []byte{0x11, 1}, partial: true, blockFlow: true, kind: gocan.ErrTransmitQueueFull},
		{name: "reset after Flow Control timeout", request: []byte{0x11, 1}, responses: [][]byte{{0x51, 1}}, want: []byte{1}},
		{name: "incomplete suppressed response", request: []byte{0x11, 0x81}, mode: "wait", partial: true, kind: isotp.ErrConsecutiveFrameTimeout},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			testerTransport.block = step.blockFlow
			callContext, cancelCall := context.WithCancel(ctx)
			if step.timeout != 0 {
				cancelCall()
				callContext, cancelCall = context.WithTimeout(ctx, step.timeout)
			}
			defer cancelCall()
			serverResult := make(chan error, 1)
			go func() {
				serverErr := receiveRequest(ctx, ecuLink, step.request)
				if serverErr == nil && step.mode != "send" && client.RetentionCursor() == capture.End() {
					t.Error("active exchange did not retain its response boundary")
				}
				if step.cancel {
					cancelCall()
				}
				if step.partial && serverErr == nil {
					serverErr = ecuBus.Send(ctx, gocan.Frame{ID: 0x7e8, DLC: 8, Data: [64]byte{0x10, 8, 0x51, 1}})
				}
				for _, reply := range step.responses {
					if serverErr == nil {
						serverErr = ecuLink.Send(ctx, reply)
					}
				}
				serverResult <- serverErr
			}()
			request := uds.Request{Service: uds.ServiceID(step.request[0]), Data: step.request[1:]}
			var response *uds.Response
			var err error
			switch step.mode {
			case "":
				var received uds.Response
				received, err = client.Do(callContext, request)
				if err == nil {
					response = &received
				}
			case "wait":
				response, err = client.DoSuppressed(callContext, request)
			default:
				err = client.Send(callContext, request)
			}
			var negative *uds.NegativeResponseError
			if step.nrc != 0 {
				if !errors.As(err, &negative) || negative.Service != request.Service || negative.Code != step.nrc {
					t.Fatalf("negative response = %v", err)
				}
			} else if !errors.Is(err, step.kind) {
				t.Fatalf("error = %v, want %v", err, step.kind)
			}
			if step.blockFlow && errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("spent retry budget reported a caller deadline: %v", err)
			}
			if (response == nil) != (step.want == nil) {
				t.Fatalf("response = %#v, want data %x (nil: %t)", response, step.want, step.want == nil)
			}
			if response != nil && (!bytes.Equal(response.Data, step.want) || response.Service != request.Service) {
				t.Fatalf("response = %#v, want service %#x data %x", response, request.Service, step.want)
			}
			if err := <-serverResult; err != nil {
				t.Fatalf("ECU: %v", err)
			}
			if client.RetentionCursor() != capture.End() {
				t.Fatal("completed exchange retained history")
			}
		})
	}

	for _, send := range []func() error{
		func() error { return functional.SendECUReset(ctx, 0x81) },
		func() error { return functional.SendCommunicationControlWithNode(ctx, 0x03, 1, 0x1234) },
		func() error { return functional.SendCommunicationControl(ctx, 0x04, 1) },
	} {
		if err := send(); err == nil {
			t.Fatal("functional send accepted an invalid subfunction")
		}
	}
	// The first broadcast also proves the rejected requests emitted no traffic.
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
			send: func() error { return functional.SendECUReset(ctx, uds.ResetHard) },
			want: []byte{0x11, 0x81},
		},
		{
			send: func() error {
				return functional.SendCommunicationControlWithNode(ctx, uds.CommunicationEnableRxDisableTxWithNode, uds.CommunicationTypeNormal, 0x1234)
			},
			want: []byte{0x28, 0x84, 0x01, 0x12, 0x34},
		},
		{
			send: func() error {
				return functional.SendCommunicationControlWithNode(ctx, uds.CommunicationEnableRxAndTxWithNode, uds.CommunicationTypeNormal, 0x1234)
			},
			want: []byte{0x28, 0x85, 0x01, 0x12, 0x34},
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

type flowControlBus struct {
	gocan.Bus
	block bool
}

func (bus *flowControlBus) Send(ctx context.Context, frame gocan.Frame) error {
	if bus.block && frame.Data[0]>>4 == 3 {
		return gocan.ErrTransmitQueueFull
	}
	return bus.Bus.Send(ctx, frame)
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
