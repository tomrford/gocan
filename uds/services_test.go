package uds_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/cdd"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/isotp"
	"github.com/tomrford/gocan/uds"
)

func TestSemanticClientLifecycle(t *testing.T) {
	client, server, ctx := newSemanticPair(t)
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- serveSemanticLifecycle(ctx, server)
	}()

	read, err := client.ReadDataByIdentifier(ctx, 0xf190)
	if err != nil || !bytes.Equal(read, []byte{0x12, 0x34}) {
		t.Fatalf("ReadDataByIdentifier = %x, %v", read, err)
	}
	if err := client.WriteDataByIdentifier(ctx, 0xf191, []byte{0x56, 0x78}); err != nil {
		t.Fatalf("WriteDataByIdentifier: %v", err)
	}

	session, err := client.DiagnosticSessionControl(ctx, uds.SessionProgramming)
	if err != nil {
		t.Fatalf("DiagnosticSessionControl: %v", err)
	}
	if session.Session != uds.SessionProgramming || session.Timing == nil ||
		session.Timing.P2ServerMax != 50*time.Millisecond ||
		session.Timing.P2StarServerMax != 5*time.Second {
		t.Fatalf("session response = %#v", session)
	}
	if err := client.ApplySessionTiming(*session.Timing); err != nil {
		t.Fatalf("ApplySessionTiming: %v", err)
	}

	seed, err := client.RequestSeed(ctx, 5, []byte{0xaa})
	if err != nil || !bytes.Equal(seed, []byte{0x12, 0x34}) {
		t.Fatalf("RequestSeed = %x, %v", seed, err)
	}
	if err := client.SendKey(ctx, 5, []byte{0xab, 0xcd}); err != nil {
		t.Fatalf("SendKey: %v", err)
	}

	if err := client.CommunicationControl(ctx, uds.CommunicationDisableRxAndTx, uds.CommunicationTypeNormalAndNetworkManagement); err != nil {
		t.Fatalf("CommunicationControl: %v", err)
	}
	if err := client.ControlDTCSetting(ctx, uds.DTCSettingOff, nil); err != nil {
		t.Fatalf("ControlDTCSetting: %v", err)
	}

	status, err := client.RoutineControl(ctx, uds.RoutineStart, 0xff00, []byte{0x01})
	if err != nil || !bytes.Equal(status, []byte{0x80}) {
		t.Fatalf("RoutineControl = %x, %v", status, err)
	}
	maximum, err := client.RequestDownload(ctx, 0, uds.MemoryLocation{
		Address:       0x12345678,
		Size:          0x100,
		AddressLength: 4,
		SizeLength:    2,
	})
	if err != nil || maximum != 0x402 {
		t.Fatalf("RequestDownload = %#x, %v", maximum, err)
	}
	transferRecord, err := client.TransferData(ctx, 1, []byte{0xde, 0xad})
	if err != nil || !bytes.Equal(transferRecord, []byte{0xaa}) {
		t.Fatalf("TransferData = %x, %v", transferRecord, err)
	}
	exitRecord, err := client.RequestTransferExit(ctx, []byte{0x99})
	if err != nil || !bytes.Equal(exitRecord, []byte{0x55}) {
		t.Fatalf("RequestTransferExit = %x, %v", exitRecord, err)
	}
	if err := client.TesterPresent(ctx); err != nil {
		t.Fatalf("TesterPresent: %v", err)
	}
	if err := client.SendTesterPresent(ctx); err != nil {
		t.Fatalf("SendTesterPresent: %v", err)
	}
	resetRecord, err := client.ECUReset(ctx, uds.ResetHard)
	if err != nil || !bytes.Equal(resetRecord, []byte{0x0a}) {
		t.Fatalf("ECUReset = %x, %v", resetRecord, err)
	}

	if err := <-serverResult; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func serveSemanticLifecycle(ctx context.Context, link *isotp.Link) error {
	exchanges := []struct {
		request  []byte
		response []byte
	}{
		{[]byte{0x22, 0xf1, 0x90}, []byte{0x62, 0xf1, 0x90, 0x12, 0x34}},
		{[]byte{0x2e, 0xf1, 0x91, 0x56, 0x78}, []byte{0x6e, 0xf1, 0x91}},
		{[]byte{0x10, 0x02}, []byte{0x50, 0x02, 0x00, 0x32, 0x01, 0xf4}},
		{[]byte{0x27, 0x05, 0xaa}, []byte{0x67, 0x05, 0x12, 0x34}},
		{[]byte{0x27, 0x06, 0xab, 0xcd}, []byte{0x67, 0x06}},
		{[]byte{0x28, 0x03, 0x03}, []byte{0x68, 0x03}},
		{[]byte{0x85, 0x02}, []byte{0xc5, 0x02}},
		{[]byte{0x31, 0x01, 0xff, 0x00, 0x01}, []byte{0x71, 0x01, 0xff, 0x00, 0x80}},
		{[]byte{0x34, 0x00, 0x24, 0x12, 0x34, 0x56, 0x78, 0x01, 0x00}, []byte{0x74, 0x20, 0x04, 0x02}},
		{[]byte{0x36, 0x01, 0xde, 0xad}, []byte{0x76, 0x01, 0xaa}},
		{[]byte{0x37, 0x99}, []byte{0x77, 0x55}},
		{[]byte{0x3e, 0x00}, []byte{0x7e, 0x00}},
	}
	for _, exchange := range exchanges {
		if err := receiveRequest(ctx, link, exchange.request); err != nil {
			return err
		}
		if err := link.Send(ctx, exchange.response); err != nil {
			return fmt.Errorf("send %x: %w", exchange.response, err)
		}
	}
	if err := receiveRequest(ctx, link, []byte{0x3e, 0x80}); err != nil {
		return err
	}
	if err := receiveRequest(ctx, link, []byte{0x11, 0x01}); err != nil {
		return err
	}
	if err := link.Send(ctx, []byte{0x51, 0x01, 0x0a}); err != nil {
		return fmt.Errorf("send reset: %w", err)
	}
	return nil
}

func TestCDDDataIdentifierLifecycle(t *testing.T) {
	database, err := cdd.ParseFile(filepath.Join("..", "cdd", "testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	thermal, ok := database.DIDByName("ThermalStatus")
	if !ok || thermal.Read == nil {
		t.Fatalf("unexpected ThermalStatus DID: %#v", thermal)
	}
	writable, ok := database.DIDByName("WritableSettings")
	if !ok || writable.Write == nil {
		t.Fatalf("unexpected WritableSettings DID: %#v", writable)
	}

	client, server, ctx := newSemanticPair(t)
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- serveCDDDataIdentifierLifecycle(ctx, server)
	}()

	payload, err := client.ReadDataByIdentifier(ctx, thermal.Identifier)
	if err != nil {
		t.Fatalf("ReadDataByIdentifier: %v", err)
	}
	values, err := thermal.Read.Decode(payload)
	if err != nil {
		t.Fatalf("decode ThermalStatus: %v", err)
	}
	if values["Heater"] != "On" || values["Coolant"] != 25.0 || values["Cycles"] != uint64(7) {
		t.Fatalf("ThermalStatus values = %#v", values)
	}

	payload, err = writable.Write.Encode(cdd.Values{"Setting": uint8(0x2a)})
	if err != nil {
		t.Fatalf("encode WritableSettings: %v", err)
	}
	if err := client.WriteDataByIdentifier(ctx, writable.Identifier, payload); err != nil {
		t.Fatalf("WriteDataByIdentifier: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func serveCDDDataIdentifierLifecycle(ctx context.Context, link *isotp.Link) error {
	if err := receiveRequest(ctx, link, []byte{0x22, 0xf1, 0x90}); err != nil {
		return err
	}
	if err := link.Send(ctx, []byte{0x62, 0xf1, 0x90, 0x01, 0x00, 0x82, 0x00, 0x07}); err != nil {
		return err
	}
	if err := receiveRequest(ctx, link, []byte{0x2e, 0xf1, 0x93, 0x2a}); err != nil {
		return err
	}
	return link.Send(ctx, []byte{0x6e, 0xf1, 0x93})
}

func TestSemanticClientRejectsInvalidInputs(t *testing.T) {
	client, _, ctx := newSemanticPair(t)
	if _, err := client.DiagnosticSessionControl(ctx, 0x83); err == nil || !strings.Contains(err.Error(), "suppressPositiveResponse") {
		t.Fatalf("suppressed session error = %v", err)
	}
	location := uds.MemoryLocation{Address: 0x100, Size: 1, AddressLength: 1, SizeLength: 1}
	if _, err := client.RequestDownload(ctx, 0, location); err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("memory location error = %v", err)
	}
	if err := client.ApplySessionTiming(uds.SessionTiming{}); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("session timing error = %v", err)
	}
	if err := client.CommunicationControl(ctx, 0x04, uds.CommunicationTypeNormal); err == nil || !strings.Contains(err.Error(), "enhanced address information") {
		t.Fatalf("enhanced communication control error = %v", err)
	}
	if err := client.CommunicationControl(ctx, uds.CommunicationEnableRxAndTx, 0x40); err == nil || !strings.Contains(err.Error(), "selects neither") {
		t.Fatalf("communication type error = %v", err)
	}
}

func TestSemanticClientValidatesResponses(t *testing.T) {
	tests := []struct {
		name     string
		request  []byte
		response []byte
		call     func(context.Context, *uds.Client) error
		kind     error
	}{
		{
			name:     "legacy session",
			request:  []byte{0x10, 0x03},
			response: []byte{0x50, 0x03},
			call: func(ctx context.Context, client *uds.Client) error {
				response, err := client.DiagnosticSessionControl(ctx, uds.SessionExtended)
				if err == nil && response.Timing != nil {
					return fmt.Errorf("legacy response has timing %#v", response.Timing)
				}
				return err
			},
		},
		{
			name:     "read identifier echo",
			request:  []byte{0x22, 0xf1, 0x90},
			response: []byte{0x62, 0xf1, 0x91, 0x00},
			call: func(ctx context.Context, client *uds.Client) error {
				_, err := client.ReadDataByIdentifier(ctx, 0xf190)
				return err
			},
			kind: uds.ErrUnexpectedResponse,
		},
		{
			name:     "download low nibble",
			request:  []byte{0x34, 0x00, 0x11, 0x10, 0x20},
			response: []byte{0x74, 0x21, 0x01, 0x00},
			call: func(ctx context.Context, client *uds.Client) error {
				_, err := client.RequestDownload(ctx, 0, uds.MemoryLocation{Address: 0x10, Size: 0x20, AddressLength: 1, SizeLength: 1})
				return err
			},
			kind: uds.ErrInvalidResponse,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, server, ctx := newSemanticPair(t)
			serverResult := make(chan error, 1)
			go func() {
				if err := receiveRequest(ctx, server, test.request); err != nil {
					serverResult <- err
					return
				}
				serverResult <- server.Send(ctx, test.response)
			}()
			err := test.call(ctx, client)
			if !errors.Is(err, test.kind) {
				t.Fatalf("error = %v, want %v", err, test.kind)
			}
			if err := <-serverResult; err != nil {
				t.Fatalf("server: %v", err)
			}
		})
	}
}

func newSemanticPair(t *testing.T) (*uds.Client, *isotp.Link, context.Context) {
	t.Helper()
	capture := gocan.NewCapture()
	var network virtual.Network
	testerBus, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "semantic tester"})
	if err != nil {
		t.Fatalf("Open tester: %v", err)
	}
	t.Cleanup(func() { _ = testerBus.Close() })
	ecuBus, err := network.Open(context.Background(), capture, virtual.Config{ID: 2, Name: "semantic ECU"})
	if err != nil {
		t.Fatalf("Open ECU: %v", err)
	}
	t.Cleanup(func() { _ = ecuBus.Close() })
	testerLink, err := isotp.New(testerBus, capture, isotp.Config{TransmitID: 0x700, ReceiveID: 0x708})
	if err != nil {
		t.Fatalf("New tester link: %v", err)
	}
	server, err := isotp.New(ecuBus, capture, isotp.Config{TransmitID: 0x708, ReceiveID: 0x700})
	if err != nil {
		t.Fatalf("New server link: %v", err)
	}
	client, err := uds.New(testerLink, uds.Config{P2Timeout: time.Second, P2StarTimeout: time.Second})
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return client, server, ctx
}
