package isotp_test

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
)

func TestExchangeRoundTrip(t *testing.T) {
	tests := []struct {
		name               string
		transmitID         uint32
		receiveID          uint32
		flags              gocan.FrameFlags
		dataLength         int
		requestLength      int
		responseLength     int
		peerBlockSize      byte
		peerSeparationTime byte
		minimumRequestGap  time.Duration
		waitBeforeContinue bool
	}{
		{
			name:               "classical",
			transmitID:         0x7e0,
			receiveID:          0x7e8,
			dataLength:         8,
			requestLength:      130,
			responseLength:     140,
			peerBlockSize:      4,
			peerSeparationTime: 5,
			minimumRequestGap:  4 * time.Millisecond,
			waitBeforeContinue: true,
		},
		{
			name:               "CAN FD extended length",
			transmitID:         0x18da10f1,
			receiveID:          0x18daf110,
			flags:              gocan.FrameExtended | gocan.FrameFD | gocan.FrameBitRateSwitch,
			dataLength:         64,
			requestLength:      5000,
			responseLength:     4200,
			peerBlockSize:      7,
			peerSeparationTime: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capture := gocan.NewCapture()
			var network virtual.Network
			tester, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
			if err != nil {
				t.Fatalf("Open tester: %v", err)
			}
			t.Cleanup(func() { _ = tester.Close() })
			ecu, err := network.Open(context.Background(), capture, virtual.Config{ID: 2, Name: "ecu"})
			if err != nil {
				t.Fatalf("Open ECU: %v", err)
			}
			t.Cleanup(func() { _ = ecu.Close() })

			link, err := isotp.New(tester, isotp.Config{
				TransmitID:               test.transmitID,
				ReceiveID:                test.receiveID,
				FrameFlags:               test.flags,
				TransmitDataLength:       uint8(test.dataLength),
				MaximumPayloadLength:     8192,
				AdvertisedBlockSize:      3,
				AdvertisedSeparationTime: 100 * time.Microsecond,
				WaitFrameLimit:           1,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			requestPayload := patternedPayload(test.requestLength, 0x22)
			responsePayload := patternedPayload(test.responseLength, 0x62)
			peer := rawPeer{
				bus:        ecu,
				capture:    capture,
				cursor:     capture.End(),
				receiveKey: gocan.FrameKey{Bus: ecu.ID(), ID: test.transmitID, Direction: gocan.DirectionReceive, Extended: test.flags.Has(gocan.FrameExtended)},
				transmitID: test.receiveID,
				flags:      test.flags,
				dataLength: test.dataLength,
				wantBlock:  3,
				wantSTmin:  0xf1,
			}
			peerErrors := make(chan error, 1)
			go func() {
				peerErrors <- peer.exchange(ctx, requestPayload, responsePayload, test.peerBlockSize, test.peerSeparationTime, test.minimumRequestGap, test.waitBeforeContinue)
			}()

			exchange, err := link.Begin(ctx, requestPayload)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer exchange.Close()

			pending, err := exchange.Next(ctx, 0)
			if err != nil {
				t.Fatalf("Next pending response: %v", err)
			}
			if want := []byte{0x7f, requestPayload[0], 0x78}; !bytes.Equal(pending, want) {
				t.Fatalf("pending response = %x, want %x", pending, want)
			}

			response, err := exchange.Next(ctx, 0)
			if err != nil {
				t.Fatalf("Next final response: %v", err)
			}
			if !bytes.Equal(response, responsePayload) {
				t.Fatalf("final response length = %d, want %d", len(response), len(responsePayload))
			}
			if err := <-peerErrors; err != nil {
				t.Fatalf("ECU: %v", err)
			}
		})
	}
}

func TestFailuresReleaseLink(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	tester, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		t.Fatalf("Open tester: %v", err)
	}
	t.Cleanup(func() { _ = tester.Close() })
	ecu, err := network.Open(context.Background(), capture, virtual.Config{ID: 2, Name: "ecu"})
	if err != nil {
		t.Fatalf("Open ECU: %v", err)
	}
	t.Cleanup(func() { _ = ecu.Close() })

	link, err := isotp.New(tester, isotp.Config{
		TransmitID:         0x7e0,
		ReceiveID:          0x7e8,
		FlowControlTimeout: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := link.Begin(context.Background(), patternedPayload(20, 1)); !errors.Is(err, isotp.ErrFlowControlTimeout) {
		t.Fatalf("Begin without Flow Control = %v, want ErrFlowControlTimeout", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	peer := rawPeer{
		bus:        ecu,
		capture:    capture,
		cursor:     capture.End(),
		receiveKey: gocan.FrameKey{Bus: ecu.ID(), ID: 0x7e0, Direction: gocan.DirectionReceive},
		transmitID: 0x7e8,
		dataLength: 8,
	}
	peerErrors := make(chan error, 1)
	go func() {
		_, err := peer.receivePayload(ctx, 0, 0, 0, false)
		if err == nil {
			err = peer.sendPayload(ctx, []byte{0x7e, 0})
		}
		peerErrors <- err
	}()
	exchange, err := link.Begin(ctx, []byte{0x3e, 0})
	if err != nil {
		t.Fatalf("Begin after timeout: %v", err)
	}
	response, err := exchange.Next(ctx, 0)
	if err != nil {
		t.Fatalf("Next response: %v", err)
	}
	if !bytes.Equal(response, []byte{0x7e, 0}) {
		t.Fatalf("response = %x", response)
	}
	exchange.Close()
	if err := <-peerErrors; err != nil {
		t.Fatalf("ECU: %v", err)
	}
}

func TestInvalidResponsesDoNotEscapeConfiguredBounds(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	tester, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		t.Fatalf("Open tester: %v", err)
	}
	t.Cleanup(func() { _ = tester.Close() })
	ecu, err := network.Open(context.Background(), capture, virtual.Config{ID: 2, Name: "ecu"})
	if err != nil {
		t.Fatalf("Open ECU: %v", err)
	}
	t.Cleanup(func() { _ = ecu.Close() })

	link, err := isotp.New(tester, isotp.Config{
		TransmitID:           0x7e0,
		ReceiveID:            0x7e8,
		MaximumPayloadLength: 4,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	peer := rawPeer{
		bus:        ecu,
		capture:    capture,
		cursor:     capture.End(),
		receiveKey: gocan.FrameKey{Bus: ecu.ID(), ID: 0x7e0, Direction: gocan.DirectionReceive},
		transmitID: 0x7e8,
		dataLength: 8,
	}
	peerErrors := make(chan error, 1)
	go func() {
		_, err := peer.receivePayload(ctx, 0, 0, 0, false)
		for _, data := range [][]byte{
			{5, 1, 2, 3, 4, 5},
			{0x10, 7, 1, 2, 3, 4, 5, 6},
			{4, 1, 2, 3, 4},
		} {
			if err == nil {
				err = peer.sendFrame(ctx, data, false)
			}
		}
		peerErrors <- err
	}()

	exchange, err := link.Begin(ctx, []byte{0x3e, 0})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer exchange.Close()
	if _, err := exchange.Next(ctx, 0); !errors.Is(err, isotp.ErrPayloadTooLarge) {
		t.Fatalf("oversized Single Frame = %v, want ErrPayloadTooLarge", err)
	}
	if _, err := exchange.Next(ctx, 0); !errors.Is(err, isotp.ErrProtocol) {
		t.Fatalf("short First Frame = %v, want ErrProtocol", err)
	}
	response, err := exchange.Next(ctx, 0)
	if err != nil {
		t.Fatalf("Next valid response: %v", err)
	}
	if !bytes.Equal(response, []byte{1, 2, 3, 4}) {
		t.Fatalf("valid response = %x", response)
	}
	if err := <-peerErrors; err != nil {
		t.Fatalf("ECU: %v", err)
	}
}

func TestExchangeStopsWithBus(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	tester, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		t.Fatalf("Open tester: %v", err)
	}

	link, err := isotp.New(tester, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	exchange, err := link.Begin(context.Background(), []byte{0x3e, 0})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := exchange.Next(context.Background(), 0)
		result <- err
	}()
	if err := tester.Close(); err != nil {
		t.Fatalf("Close bus: %v", err)
	}
	if err := <-result; !errors.Is(err, gocan.ErrBusClosed) {
		t.Fatalf("Next after bus close = %v, want ErrBusClosed", err)
	}
	exchange.Close()
}

// TestSendAndReceivePairedLinks drives both state machines through the public
// server-style API and checks that successive payloads are neither dropped nor
// repeated, which is what one receive position per Link has to guarantee.
func TestSendAndReceivePairedLinks(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	first, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "first"})
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := network.Open(context.Background(), capture, virtual.Config{ID: 2, Name: "second"})
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	third, err := network.Open(context.Background(), capture, virtual.Config{ID: 3, Name: "third"})
	if err != nil {
		t.Fatalf("Open third: %v", err)
	}
	t.Cleanup(func() { _ = third.Close() })

	sender, err := isotp.New(first, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
	if err != nil {
		t.Fatalf("New sender: %v", err)
	}
	receiver, err := isotp.New(second, isotp.Config{TransmitID: 0x7e8, ReceiveID: 0x7e0, AdvertisedBlockSize: 2})
	if err != nil {
		t.Fatalf("New receiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payloads := [][]byte{
		patternedPayload(4, 0x10),
		patternedPayload(64, 0x20),
		patternedPayload(7, 0x30),
	}

	received := make(chan [][]byte, 1)
	receiveErrors := make(chan error, 1)
	go func() {
		var all [][]byte
		for range payloads {
			payload, err := receiver.Receive(ctx)
			if err != nil {
				receiveErrors <- err
				return
			}
			all = append(all, payload)
		}
		received <- all
		receiveErrors <- nil
	}()

	for index, payload := range payloads {
		if err := sender.Send(ctx, payload); err != nil {
			t.Fatalf("Send payload %d: %v", index, err)
		}
	}
	if err := <-receiveErrors; err != nil {
		t.Fatalf("Receive: %v", err)
	}
	for index, payload := range <-received {
		if !bytes.Equal(payload, payloads[index]) {
			t.Fatalf("payload %d = %x, want %x", index, payload, payloads[index])
		}
	}

	functionalReceivers := make([]*isotp.Link, 2)
	for index, bus := range []gocan.Bus{second, third} {
		functionalReceivers[index], err = isotp.New(bus, isotp.Config{TransmitID: 0x7e8 + uint32(index), ReceiveID: 0x7df})
		if err != nil {
			t.Fatalf("New functional receiver %d: %v", index, err)
		}
	}
	functional, err := isotp.NewFunctional(first, isotp.FunctionalConfig{TransmitID: 0x7df})
	if err != nil {
		t.Fatalf("NewFunctional: %v", err)
	}
	broadcast := patternedPayload(7, 0x3e)
	if err := functional.Send(ctx, broadcast); err != nil {
		t.Fatalf("Send functional payload: %v", err)
	}
	for index, receiver := range functionalReceivers {
		payload, err := receiver.Receive(ctx)
		if err != nil || !bytes.Equal(payload, broadcast) {
			t.Fatalf("functional receiver %d received %x, %v", index, payload, err)
		}
	}
	if err := functional.Send(ctx, patternedPayload(8, 0x3e)); !errors.Is(err, isotp.ErrPayloadTooLarge) {
		t.Fatalf("oversized functional Send = %v, want ErrPayloadTooLarge", err)
	}

	functionalFD, err := isotp.NewFunctional(first, isotp.FunctionalConfig{
		TransmitID:         0x7df,
		FrameFlags:         gocan.FrameFD,
		TransmitDataLength: 64,
		PadFrames:          true,
		PaddingByte:        0xcc,
	})
	if err != nil {
		t.Fatalf("NewFunctional FD: %v", err)
	}
	broadcast = patternedPayload(62, 0x2e)
	if err := functionalFD.Send(ctx, broadcast); err != nil {
		t.Fatalf("Send functional FD payload: %v", err)
	}
	payload, err := functionalReceivers[0].Receive(ctx)
	if err != nil || !bytes.Equal(payload, broadcast) {
		t.Fatalf("functional receiver received %x, %v", payload, err)
	}
	if err := functionalFD.Send(ctx, patternedPayload(63, 0x2e)); !errors.Is(err, isotp.ErrPayloadTooLarge) {
		t.Fatalf("oversized functional FD Send = %v, want ErrPayloadTooLarge", err)
	}
}

// TestCloseCancelsPendingNext asserts that Close does not wait out a protocol
// timeout, so `defer exchange.Close()` is safe on every path.
func TestCloseCancelsPendingNext(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	tester, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		t.Fatalf("Open tester: %v", err)
	}
	t.Cleanup(func() { _ = tester.Close() })
	ecu, err := network.Open(context.Background(), capture, virtual.Config{ID: 2, Name: "ecu"})
	if err != nil {
		t.Fatalf("Open ECU: %v", err)
	}
	t.Cleanup(func() { _ = ecu.Close() })

	link, err := isotp.New(tester, isotp.Config{
		TransmitID:              0x7e0,
		ReceiveID:               0x7e8,
		ConsecutiveFrameTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	peer := rawPeer{
		bus:        ecu,
		capture:    capture,
		cursor:     capture.End(),
		receiveKey: gocan.FrameKey{Bus: ecu.ID(), ID: 0x7e0, Direction: gocan.DirectionReceive},
		transmitID: 0x7e8,
		dataLength: 8,
	}
	exchange, err := link.Begin(ctx, []byte{0x3e, 0})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	pending := make(chan error, 1)
	go func() {
		_, err := exchange.Next(context.Background(), 0)
		pending <- err
	}()

	// Drive Next into reassembly so that it is provably blocked rather than
	// merely scheduled: it has to answer a First Frame with Flow Control before
	// it can wait for a Consecutive Frame that never arrives.
	if _, err := peer.nextFrame(ctx); err != nil {
		t.Fatalf("peer read request: %v", err)
	}
	if err := peer.sendFrame(ctx, []byte{0x10, 20, 1, 2, 3, 4, 5, 6}, true); err != nil {
		t.Fatalf("peer send First Frame: %v", err)
	}
	control, err := peer.nextFrame(ctx)
	if err != nil {
		t.Fatalf("peer read Flow Control: %v", err)
	}
	if got := control.Frame.Data[0]; got != 0x30 {
		t.Fatalf("peer read %#x, want a Continue Flow Control", got)
	}

	closed := make(chan struct{})
	go func() {
		exchange.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while Next was pending")
	}
	if err := <-pending; !errors.Is(err, isotp.ErrExchangeClosed) {
		t.Fatalf("pending Next = %v, want ErrExchangeClosed", err)
	}
	if _, err := exchange.Next(context.Background(), 0); !errors.Is(err, isotp.ErrExchangeClosed) {
		t.Fatalf("Next after Close = %v, want ErrExchangeClosed", err)
	}

	// The link must be usable again.
	if _, err := link.Begin(context.Background(), []byte{0x3e, 0}); err != nil {
		t.Fatalf("Begin after Close: %v", err)
	}
}

// TestSegmentationConformance covers two ISO 15765-2 rules that pull in opposite
// directions: a reserved separation time must slow a transmission rather than
// abandon it, while a Consecutive Frame that is short without being the last one
// must be rejected.
func TestSegmentationConformance(t *testing.T) {
	newPair := func(t *testing.T) (*isotp.Link, *rawPeer, *gocan.Capture) {
		t.Helper()
		capture := gocan.NewCapture()
		var network virtual.Network
		tester, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "tester"})
		if err != nil {
			t.Fatalf("Open tester: %v", err)
		}
		t.Cleanup(func() { _ = tester.Close() })
		ecu, err := network.Open(context.Background(), capture, virtual.Config{ID: 2, Name: "ecu"})
		if err != nil {
			t.Fatalf("Open ECU: %v", err)
		}
		t.Cleanup(func() { _ = ecu.Close() })

		link, err := isotp.New(tester, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return link, &rawPeer{
			bus:        ecu,
			capture:    capture,
			cursor:     capture.End(),
			receiveKey: gocan.FrameKey{Bus: ecu.ID(), ID: 0x7e0, Direction: gocan.DirectionReceive},
			transmitID: 0x7e8,
			dataLength: 8,
		}, capture
	}

	t.Run("reserved separation time slows instead of failing", func(t *testing.T) {
		link, peer, _ := newPair(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		payload := patternedPayload(20, 0x2e)
		received := make(chan []byte, 1)
		peerErrors := make(chan error, 1)
		go func() {
			// 0x80 is reserved, so the sender must fall back to 127 ms.
			got, err := peer.receivePayload(ctx, 0, 0x80, 120*time.Millisecond, false)
			received <- got
			peerErrors <- err
		}()
		if err := link.Send(ctx, payload); err != nil {
			t.Fatalf("Send with reserved STmin: %v", err)
		}
		if err := <-peerErrors; err != nil {
			t.Fatalf("ECU: %v", err)
		}
		if got := <-received; !bytes.Equal(got, payload) {
			t.Fatalf("received %d bytes, want %d", len(got), len(payload))
		}
	})

	t.Run("short non-final Consecutive Frame is rejected", func(t *testing.T) {
		link, peer, _ := newPair(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		peerErrors := make(chan error, 1)
		go func() {
			err := peer.sendFrame(ctx, []byte{0x10, 20, 1, 2, 3, 4, 5, 6}, true)
			if err == nil {
				_, err = peer.nextFrame(ctx) // the receiver's Flow Control
			}
			if err == nil {
				err = peer.sendFrame(ctx, []byte{0x21, 7}, false)
			}
			peerErrors <- err
		}()
		if _, err := link.Receive(ctx); !errors.Is(err, isotp.ErrProtocol) {
			t.Fatalf("short Consecutive Frame = %v, want ErrProtocol", err)
		}
		if err := <-peerErrors; err != nil {
			t.Fatalf("ECU: %v", err)
		}
	})
}

type rawPeer struct {
	bus        gocan.Bus
	capture    *gocan.Capture
	cursor     gocan.Cursor
	receiveKey gocan.FrameKey
	transmitID uint32
	flags      gocan.FrameFlags
	dataLength int
	wantBlock  byte
	wantSTmin  byte
}

func (peer *rawPeer) exchange(ctx context.Context, request, response []byte, blockSize, separationTime byte, minimumGap time.Duration, wait bool) error {
	received, err := peer.receivePayload(ctx, blockSize, separationTime, minimumGap, wait)
	if err != nil {
		return err
	}
	if !bytes.Equal(received, request) {
		return fmt.Errorf("request length = %d, want %d", len(received), len(request))
	}
	if err := peer.sendPayload(ctx, []byte{0x7f, request[0], 0x78}); err != nil {
		return err
	}
	return peer.sendPayload(ctx, response)
}

func (peer *rawPeer) receivePayload(ctx context.Context, blockSize, separationTime byte, minimumGap time.Duration, wait bool) ([]byte, error) {
	frame, err := peer.nextFrame(ctx)
	if err != nil {
		return nil, err
	}
	data := frame.Frame.Data[:frame.Frame.DataLength()]
	switch data[0] >> 4 {
	case 0:
		start, length := 1, int(data[0]&0x0f)
		if length == 0 {
			start, length = 2, int(data[1])
		}
		return append([]byte(nil), data[start:start+length]...), nil
	case 1:
	default:
		return nil, fmt.Errorf("peer expected Single or First Frame, got %#x", data[0]>>4)
	}

	total := int(data[0]&0x0f)<<8 | int(data[1])
	start := 2
	if total == 0 {
		total = int(uint32(data[2])<<24 | uint32(data[3])<<16 | uint32(data[4])<<8 | uint32(data[5]))
		start = 6
	}
	payload := append([]byte(nil), data[start:]...)
	if wait {
		if err := peer.sendFlowControl(ctx, 1, blockSize, separationTime); err != nil {
			return nil, err
		}
	}
	if err := peer.sendFlowControl(ctx, 0, blockSize, separationTime); err != nil {
		return nil, err
	}

	sequence, blockCount := byte(1), byte(0)
	var previousFrameTime time.Time
	for len(payload) < total {
		frame, err = peer.nextFrame(ctx)
		if err != nil {
			return nil, err
		}
		data = frame.Frame.Data[:frame.Frame.DataLength()]
		if data[0]>>4 != 2 || data[0]&0x0f != sequence {
			return nil, fmt.Errorf("peer received invalid Consecutive Frame %#x", data[0])
		}
		if minimumGap > 0 && !previousFrameTime.IsZero() && frame.Timestamp.Sub(previousFrameTime) < minimumGap {
			return nil, fmt.Errorf("peer received Consecutive Frames %s apart, want at least %s", frame.Timestamp.Sub(previousFrameTime), minimumGap)
		}
		previousFrameTime = frame.Timestamp
		remaining := total - len(payload)
		part := data[1:]
		if len(part) > remaining {
			part = part[:remaining]
		}
		payload = append(payload, part...)
		sequence = (sequence + 1) & 0x0f
		blockCount++
		if blockSize > 0 && blockCount == blockSize && len(payload) < total {
			if err := peer.sendFlowControl(ctx, 0, blockSize, separationTime); err != nil {
				return nil, err
			}
			blockCount = 0
		}
	}
	return payload, nil
}

func (peer *rawPeer) sendPayload(ctx context.Context, payload []byte) error {
	escapeSingle := len(payload) > 7
	headerLength := 1
	if escapeSingle {
		headerLength = 2
	}
	if len(payload) <= peer.dataLength-headerLength {
		data := make([]byte, headerLength+len(payload))
		if escapeSingle {
			data[1] = byte(len(payload))
		} else {
			data[0] = byte(len(payload))
		}
		copy(data[headerLength:], payload)
		return peer.sendFrame(ctx, data, false)
	}

	headerLength = 2
	if len(payload) > 0x0fff {
		headerLength = 6
	}
	data := make([]byte, peer.dataLength)
	if headerLength == 2 {
		data[0], data[1] = 0x10|byte(len(payload)>>8), byte(len(payload))
	} else {
		length := uint32(len(payload))
		data[0], data[1] = 0x10, 0
		data[2], data[3], data[4], data[5] = byte(length>>24), byte(length>>16), byte(length>>8), byte(length)
	}
	offset := copy(data[headerLength:], payload)
	if err := peer.sendFrame(ctx, data, true); err != nil {
		return err
	}

	sequence := byte(1)
	for offset < len(payload) {
		control, err := peer.nextFrame(ctx)
		if err != nil {
			return err
		}
		controlData := control.Frame.Data[:control.Frame.DataLength()]
		if len(controlData) < 3 || controlData[0] != 0x30 {
			return fmt.Errorf("peer expected Flow Control, got %x", controlData)
		}
		if controlData[1] != peer.wantBlock || controlData[2] != peer.wantSTmin {
			return fmt.Errorf("peer received Flow Control %x, want block size %#x and STmin %#x", controlData, peer.wantBlock, peer.wantSTmin)
		}
		blockSize := controlData[1]
		blockCount := byte(0)
		for offset < len(payload) && (blockSize == 0 || blockCount < blockSize) {
			end := min(offset+peer.dataLength-1, len(payload))
			data = make([]byte, 1+end-offset)
			data[0] = 0x20 | sequence
			copy(data[1:], payload[offset:end])
			if err := peer.sendFrame(ctx, data, false); err != nil {
				return err
			}
			offset = end
			sequence = (sequence + 1) & 0x0f
			blockCount++
		}
	}
	return nil
}

func (peer *rawPeer) nextFrame(ctx context.Context) (gocan.FrameEvent, error) {
	frame, cursor, err := peer.capture.Next(ctx, peer.receiveKey, peer.cursor)
	if err == nil {
		peer.cursor = cursor
	}
	return frame, err
}

func (peer *rawPeer) sendFlowControl(ctx context.Context, status, blockSize, separationTime byte) error {
	return peer.sendFrame(ctx, []byte{0x30 | status, blockSize, separationTime}, false)
}

func (peer *rawPeer) sendFrame(ctx context.Context, data []byte, full bool) error {
	targetLength := len(data)
	if full {
		targetLength = peer.dataLength
	} else if peer.flags.Has(gocan.FrameFD) {
		targetLength = testFDLength(targetLength)
	}
	padded := make([]byte, targetLength)
	copy(padded, data)
	frame, err := gocan.NewFrame(peer.transmitID, padded, peer.flags)
	if err != nil {
		return err
	}
	return peer.bus.Send(ctx, frame)
}

func patternedPayload(length int, first byte) []byte {
	payload := make([]byte, length)
	for index := range payload {
		payload[index] = byte(index)
	}
	payload[0] = first
	return payload
}

func testFDLength(length int) int {
	for _, candidate := range []int{1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 20, 24, 32, 48, 64} {
		if length <= candidate {
			return candidate
		}
	}
	return 64
}

// TestReceiveResynchronisesAfterCaptureClear covers a server link whose
// capture is reset while it is idle. The first Receive reports the discarded
// position; the next one must still see a request retained after the reset.
func TestReceiveResynchronisesAfterCaptureClear(t *testing.T) {
	capture := gocan.NewCapture()
	var network virtual.Network
	first, err := network.Open(context.Background(), capture, virtual.Config{ID: 1, Name: "first"})
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := network.Open(context.Background(), capture, virtual.Config{ID: 2, Name: "second"})
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	sender, err := isotp.New(first, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
	if err != nil {
		t.Fatalf("New sender: %v", err)
	}
	receiver, err := isotp.New(second, isotp.Config{TransmitID: 0x7e8, ReceiveID: 0x7e0})
	if err != nil {
		t.Fatalf("New receiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// One payload first, so the link holds a real position rather than the zero
	// Cursor that New starts from.
	opening := patternedPayload(4, 0x40)
	if err := sender.Send(ctx, opening); err != nil {
		t.Fatalf("Send opening payload: %v", err)
	}
	if got, err := receiver.Receive(ctx); err != nil || !bytes.Equal(got, opening) {
		t.Fatalf("Receive opening payload = %x, %v", got, err)
	}

	capture.Clear()
	payload := patternedPayload(5, 0x50)
	if err := sender.Send(ctx, payload); err != nil {
		t.Fatalf("Send after Clear: %v", err)
	}
	if _, err := receiver.Receive(ctx); !errors.Is(err, gocan.ErrCursorOutOfRange) {
		t.Fatalf("first Receive after Clear = %v, want gocan.ErrCursorOutOfRange", err)
	}
	got, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatalf("resynchronised Receive: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("resynchronised payload = %x, want %x", got, payload)
	}
}
