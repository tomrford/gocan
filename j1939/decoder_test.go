package j1939_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/dbc"
	"github.com/tomrford/gocan/j1939"
)

func TestDecoderEmitsSingleFrameMessage(t *testing.T) {
	event := frameEvent(t, 0, 0x18ea2a80, []byte{1, 2, 3})

	var decoder j1939.Decoder
	message, complete, err := decoder.Push(event)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !complete {
		t.Fatal("Push did not complete the single-frame message")
	}
	if message.PGN != 0xea00 || message.Source != 0x80 || message.Destination != 0x2a {
		t.Fatalf("message identity = %#v", message)
	}
	if want := []byte{1, 2, 3}; !reflect.DeepEqual(message.Payload, want) {
		t.Fatalf("payload = % x, want % x", message.Payload, want)
	}
}

func TestDecoderReassemblesBAMForDBC(t *testing.T) {
	const source = "BU_: ECU\n" +
		"BO_ 2566834942 LongJ1939: 20 ECU\n" +
		" SG_ Near : 0|8@1+ (1,0) [0|255] \"\" ECU\n" +
		" SG_ Far : 144|16@1+ (1,0) [0|65535] \"\" ECU\n" +
		"BA_DEF_ BO_ \"VFrameFormat\" ENUM \"StandardCAN\",\"ExtendedCAN\",\"reserved\",\"J1939PG\";\n" +
		"BA_ \"VFrameFormat\" BO_ 2566834942 3;\n"
	database, err := dbc.Parse("long-j1939.dbc", source)
	if err != nil {
		t.Fatalf("Parse DBC: %v", err)
	}
	matches := database.MessagesByPGN(0xfeca)
	if len(matches) != 1 || matches[0].Name != "LongJ1939" {
		t.Fatalf("MessagesByPGN = %#v", matches)
	}

	payload := []byte{
		0x42, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06,
		0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d,
		0x0e, 0x0f, 0x10, 0x11, 0x34, 0x12,
	}
	frames := []gocan.FrameEvent{
		frameEvent(t, 0, 0x18ecfffe, []byte{0x20, 20, 0, 3, 0xff, 0xca, 0xfe, 0x00}),
		frameEvent(t, 1, 0x18ebfffe, append([]byte{1}, payload[:7]...)),
		frameEvent(t, 2, 0x18ebfffe, append([]byte{2}, payload[7:14]...)),
		frameEvent(t, 3, 0x18ebfffe, append(append([]byte{3}, payload[14:]...), 0xff)),
	}

	capture := gocan.NewCapture()
	cursor := capture.End()
	for _, event := range frames[:2] {
		if err := capture.Append(event); err != nil {
			t.Fatalf("Append first batch: %v", err)
		}
	}
	first, cursor, err := capture.FramesSince(cursor)
	if err != nil {
		t.Fatalf("FramesSince first batch: %v", err)
	}
	var decoder j1939.Decoder
	messages, diagnostics := decoder.PushBatch(first)
	if len(messages) != 0 || len(diagnostics) != 0 {
		t.Fatalf("first PushBatch = %v, %v", messages, diagnostics)
	}

	for _, event := range frames[2:] {
		if err := capture.Append(event); err != nil {
			t.Fatalf("Append second batch: %v", err)
		}
	}
	second, _, err := capture.FramesSince(cursor)
	if err != nil {
		t.Fatalf("FramesSince second batch: %v", err)
	}
	messages, diagnostics = decoder.PushBatch(second)
	if len(diagnostics) != 0 {
		t.Fatalf("PushBatch diagnostics = %v", diagnostics)
	}
	if len(messages) != 1 {
		t.Fatalf("PushBatch returned %d messages", len(messages))
	}
	message := messages[0]
	if message.PGN != 0xfeca || message.Source != 0xfe || message.Destination != j1939.GlobalAddress {
		t.Fatalf("message identity = %#v", message)
	}
	if !reflect.DeepEqual(message.Payload, payload) {
		t.Fatalf("payload = % x, want % x", message.Payload, payload)
	}
	far, err := matches[0].DecodePayload(message.Payload, "Far")
	if err != nil {
		t.Fatalf("DecodePayload Far: %v", err)
	}
	if far != uint64(0x1234) {
		t.Fatalf("Far = %#v, want %#x", far, 0x1234)
	}
}

func TestDecoderReassemblesConnectionManagedTransfer(t *testing.T) {
	frames := []gocan.FrameEvent{
		frameEvent(t, 0, 0x18ec2180, []byte{0x10, 9, 0, 2, 1, 0x00, 0xda, 0x00}),
		// Passive reassembly observes but does not act on the receiver's CTS.
		frameEvent(t, 1, 0x18ec8021, []byte{0x11, 1, 1, 0xff, 0xff, 0x00, 0xda, 0x00}),
		frameEvent(t, 2, 0x18eb2180, []byte{1, 0, 1, 2, 3, 4, 5, 6}),
		frameEvent(t, 3, 0x18eb2180, []byte{2, 7, 8, 0xff, 0xff, 0xff, 0xff, 0xff}),
	}

	var decoder j1939.Decoder
	messages, diagnostics := decoder.PushBatch(frames)
	if len(diagnostics) != 0 {
		t.Fatalf("PushBatch diagnostics = %v", diagnostics)
	}
	if len(messages) != 1 {
		t.Fatalf("PushBatch returned %d messages", len(messages))
	}
	message := messages[0]
	if message.PGN != 0xda00 || message.Source != 0x80 || message.Destination != 0x21 {
		t.Fatalf("message identity = %#v", message)
	}
	if want := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8}; !reflect.DeepEqual(message.Payload, want) {
		t.Fatalf("payload = % x, want % x", message.Payload, want)
	}
}

func TestDecoderReportsBrokenTransportLifecycle(t *testing.T) {
	start := frameEvent(t, 0, 0x18ecff80, []byte{0x20, 9, 0, 2, 0xff, 0xca, 0xfe, 0x00})
	wrongSequence := frameEvent(t, 1, 0x18ebff80, []byte{2, 0, 1, 2, 3, 4, 5, 6})

	var decoder j1939.Decoder
	_, diagnostics := decoder.PushBatch([]gocan.FrameEvent{start, wrongSequence})
	if len(diagnostics) != 1 || !errors.Is(diagnostics[0], j1939.ErrProtocol) {
		t.Fatalf("sequence diagnostics = %v", diagnostics)
	}

	decoder.PushBatch([]gocan.FrameEvent{start})
	flushed := decoder.Flush()
	if len(flushed) != 1 || !errors.Is(flushed[0], j1939.ErrProtocol) {
		t.Fatalf("Flush diagnostics = %v", flushed)
	}

	decoder.PushBatch([]gocan.FrameEvent{start})
	abort := frameEvent(t, 2, 0x18ec80ff, []byte{0xff, 3, 0xff, 0xff, 0xff, 0xca, 0xfe, 0x00})
	_, diagnostics = decoder.PushBatch([]gocan.FrameEvent{abort})
	if len(diagnostics) != 1 || !errors.Is(diagnostics[0], j1939.ErrProtocol) {
		t.Fatalf("abort diagnostics = %v", diagnostics)
	}
}

func frameEvent(t *testing.T, offset time.Duration, id uint32, data []byte) gocan.FrameEvent {
	t.Helper()
	frame, err := gocan.NewFrame(id, data, gocan.FrameExtended)
	if err != nil {
		t.Fatalf("NewFrame(%#x): %v", id, err)
	}
	return gocan.FrameEvent{
		Bus:       1,
		Timestamp: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC).Add(offset * time.Millisecond),
		Direction: gocan.DirectionReceive,
		Frame:     frame,
	}
}
