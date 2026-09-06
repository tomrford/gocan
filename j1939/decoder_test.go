package j1939_test

import (
	"errors"
	"fmt"
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
		frameEvent(t, 0, 0x18ecff80, []byte{0x20, 20, 0, 3, 0xff, 0xca, 0xfe, 0x00}),
		frameEvent(t, 50, 0x18ebff80, append([]byte{1}, payload[:7]...)),
		frameEvent(t, 100, 0x18ebff80, append([]byte{2}, payload[7:14]...)),
		frameEvent(t, 150, 0x18ebff80, append(append([]byte{3}, payload[14:]...), 0xff)),
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
	if message.PGN != 0xfeca || message.Source != 0x80 || message.Destination != j1939.GlobalAddress {
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
		frameEvent(t, 3, 0x18ec8021, []byte{0x11, 1, 2, 0xff, 0xff, 0x00, 0xda, 0x00}),
		frameEvent(t, 4, 0x18eb2180, []byte{2, 7, 8, 0xff, 0xff, 0xff, 0xff, 0xff}),
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

func TestDecoderReferenceTransportAndRecovery(t *testing.T) {
	// Independent TTI2 fixture: python-can-j1939 01aba95cca43847bc272ec7ddb12048f587e6554,
	// test/test_ecu.py, test_broadcast_receive_long and test_peer_to_peer_receive_long.
	for _, destination := range []byte{0xff, 2} {
		t.Run(fmt.Sprintf("destination_%02x", destination), func(t *testing.T) {
			// Unused receive bytes need not contain the FF emitted by our sender.
			start := frameEvent(t, 0, 0x00ecff01, []byte{32, 20, 0, 3, 0, 176, 254, 0})
			if destination == 2 {
				start.Frame.ID = 0x00ec0201
				start.Frame.Data[0] = 16
				start.Frame.Data[4] = 1
			}
			var decoder j1939.Decoder
			if _, _, err := decoder.Push(start); err != nil {
				t.Fatal(err)
			}
			var message j1939.Message
			for packet := byte(1); packet <= 3; packet++ {
				if destination == 2 {
					cts := frameEvent(t, time.Duration(packet)*50-1, 0x1cec0102, []byte{17, 1, packet, 0, 0, 176, 254, 0})
					cts.Direction = gocan.DirectionTransmit
					if _, _, err := decoder.Push(cts); err != nil {
						t.Fatal(err)
					}
				}
				id := uint32(0x00eb0001) | uint32(destination)<<8
				data := []byte{packet, 1, 2, 3, 4, 5, 6, 7}
				if packet == 3 {
					data[7] = 255
				}
				got, complete, err := decoder.Push(frameEvent(t, time.Duration(packet)*50, id, data))
				if err != nil || complete != (packet == 3) {
					t.Fatalf("packet %d: complete=%v err=%v", packet, complete, err)
				}
				if complete {
					message = got
				}
			}
			want := []byte{1, 2, 3, 4, 5, 6, 7, 1, 2, 3, 4, 5, 6, 7, 1, 2, 3, 4, 5, 6}
			if message.PGN != 65200 || !reflect.DeepEqual(message.Payload, want) {
				t.Fatalf("message = %#v", message)
			}
			if destination == 2 {
				ack := frameEvent(t, 151, 0x1cec0102, []byte{19, 20, 0, 3, 0, 176, 254, 0})
				ack.Direction = gocan.DirectionTransmit
				if _, complete, err := decoder.Push(ack); err != nil || complete {
					t.Fatalf("EOMA: %v, %v", complete, err)
				}
			}
		})
	}
}

func TestDecoderDiscardsInvalidatedPayloads(t *testing.T) {
	start := frameEvent(t, 0, 0x18ecff80, []byte{32, 9, 0, 2, 255, 202, 254, 0})
	first := frameEvent(t, 50, 0x18ebff80, []byte{1, 1, 2, 3, 4, 5, 6, 7})
	last := frameEvent(t, 100, 0x18ebff80, []byte{2, 8, 9, 255, 255, 255, 255, 255})
	for _, test := range []struct {
		name   string
		broken gocan.FrameEvent
	}{
		{"short DT", frameEvent(t, 60, 0x18ebff80, []byte{2, 8, 9})},
		{"malformed replacement", frameEvent(t, 60, 0x18ecff80, []byte{32, 8, 0, 2, 255, 202, 254, 0})},
		{"duplicate DT", frameEvent(t, 60, 0x18ebff80, []byte{1, 1, 2, 3, 4, 5, 6, 7})},
		{"timeout", frameEvent(t, 801, 0x18feee81, []byte{42})},
		{"backwards timestamp", frameEvent(t, 49, 0x18feee81, []byte{42})},
	} {
		t.Run(test.name, func(t *testing.T) {
			var decoder j1939.Decoder
			decoder.PushBatch([]gocan.FrameEvent{start, first})
			messages, diagnostics := decoder.PushBatch([]gocan.FrameEvent{test.broken})
			if len(diagnostics) == 0 {
				t.Fatal("missing diagnostic")
			}
			if test.broken.Frame.ID == 0x18feee81 && len(messages) != 1 {
				t.Fatal("expiry swallowed unrelated message")
			}
			if _, complete, err := decoder.Push(last); complete || !errors.Is(err, j1939.ErrProtocol) {
				t.Fatalf("stale completion=%v err=%v", complete, err)
			}
			messages, diagnostics = decoder.PushBatch([]gocan.FrameEvent{start, first, last})
			if len(messages) != 1 || len(diagnostics) != 0 {
				t.Fatalf("recovery=%v, %v", messages, diagnostics)
			}
		})
	}
	// Capture loss must invalidate both TP history and identity history before replay.
	capture := gocan.NewCapture()
	capture.Append(start)
	frames, cursor, _ := capture.FramesSince(gocan.Cursor{})
	var decoder j1939.Decoder
	decoder.PushBatch(frames)
	capture.Clear()
	capture.Append(last)
	if _, _, err := capture.FramesSince(cursor); !errors.Is(err, gocan.ErrCursorOutOfRange) {
		t.Fatalf("lost cursor: %v", err)
	}
	decoder.Reset()
	frames, _, _ = capture.FramesSince(gocan.Cursor{})
	if messages, diagnostics := decoder.PushBatch(frames); len(messages) != 0 || len(diagnostics) != 1 {
		t.Fatalf("lost history=%v, %v", messages, diagnostics)
	}
}

func TestDecoderCTSRetryAndPause(t *testing.T) {
	var decoder j1939.Decoder
	frames := []gocan.FrameEvent{
		frameEvent(t, 0, 0x18ec2180, []byte{16, 9, 0, 2, 2, 0, 218, 0}),
		frameEvent(t, 1, 0x18ec8021, []byte{17, 1, 1, 255, 255, 0, 218, 0}),
		frameEvent(t, 2, 0x18eb2180, []byte{1, 99, 99, 99, 99, 99, 99, 99}),
		frameEvent(t, 3, 0x18ec8021, []byte{17, 0, 255, 255, 255, 0, 218, 0}),
		frameEvent(t, 1000, 0x18ec8021, []byte{17, 2, 1, 255, 255, 0, 218, 0}),
		// First DT after CTS uses T2 (1250ms), then T1 (750ms) between DTs.
		frameEvent(t, 2000, 0x18eb2180, []byte{1, 1, 2, 3, 4, 5, 6, 7}),
		frameEvent(t, 2700, 0x18eb2180, []byte{2, 8, 9, 255, 255, 255, 255, 255}),
	}
	messages, diagnostics := decoder.PushBatch(frames)
	if len(diagnostics) != 0 || len(messages) != 1 || !reflect.DeepEqual(messages[0].Payload, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Fatalf("retry=%v, %v", messages, diagnostics)
	}
	decoder.PushBatch(frames[:6])
	late := frames[6]
	late.Timestamp = frames[5].Timestamp.Add(751 * time.Millisecond)
	if _, complete, err := decoder.Push(late); complete || !errors.Is(err, j1939.ErrProtocol) {
		t.Fatalf("late DT: %v, %v", complete, err)
	}
	decoder.PushBatch(frames[:4])
	duringPause := frames[2]
	duringPause.Timestamp = frames[3].Timestamp
	if _, complete, err := decoder.Push(duringPause); complete || !errors.Is(err, j1939.ErrProtocol) {
		t.Fatalf("DT during pause: %v, %v", complete, err)
	}
}

func TestDecoderRejectsCTSBeforeWindowCompletes(t *testing.T) {
	var decoder j1939.Decoder
	frames := []gocan.FrameEvent{
		frameEvent(t, 0, 0x18ec2180, []byte{16, 20, 0, 3, 3, 0xca, 0xfe, 0}),
		frameEvent(t, 1, 0x18ec8021, []byte{17, 3, 1, 255, 255, 0xca, 0xfe, 0}),
		frameEvent(t, 2, 0x18eb2180, []byte{1, 1, 2, 3, 4, 5, 6, 7}),
		frameEvent(t, 3, 0x18ec8021, []byte{17, 2, 2, 255, 255, 0xca, 0xfe, 0}),
		frameEvent(t, 4, 0x18eb2180, []byte{2, 8, 9, 10, 11, 12, 13, 14}),
		frameEvent(t, 5, 0x18eb2180, []byte{3, 15, 16, 17, 18, 19, 20, 255}),
	}
	messages, diagnostics := decoder.PushBatch(frames)
	if len(messages) != 0 || len(diagnostics) != 3 || diagnostics[0].Timestamp != frames[3].Timestamp || !errors.Is(diagnostics[0], j1939.ErrProtocol) {
		t.Fatalf("overlapping CTS: %v, %v", messages, diagnostics)
	}
	// The invalid grant must not prevent a fresh session from succeeding.
	messages, diagnostics = decoder.PushBatch(append(frames[:3:3], frames[4:]...))
	if len(messages) != 1 || len(diagnostics) != 0 {
		t.Fatalf("recovery: %v, %v", messages, diagnostics)
	}
}

func TestDecoderInterleavedMaximumBAM(t *testing.T) {
	// J1939-21 permits 255 packets of 7 bytes. Keep the final packet number
	// and padding bytes in the payload, and isolate peers, buses and directions.
	var decoder j1939.Decoder
	start := frameEvent(t, 0, 0x18ecff80, []byte{32, 249, 6, 255, 255, 202, 254, 0})
	otherSource := frameEvent(t, 0, 0x18ecff81, []byte{32, 9, 0, 2, 255, 176, 254, 0})
	otherBus := otherSource
	otherBus.Bus = 2
	otherDirection := otherSource
	otherDirection.Direction = gocan.DirectionTransmit
	if _, diagnostics := decoder.PushBatch([]gocan.FrameEvent{start, otherSource, otherBus, otherDirection}); len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	for _, stream := range []gocan.FrameEvent{otherSource, otherBus, otherDirection} {
		stream.Frame.ID = 0x18ebff81
		stream.Frame.Data = [64]byte{1, 1, 2, 3, 4, 5, 6, 7}
		stream.Timestamp = stream.Timestamp.Add(time.Millisecond)
		if _, _, err := decoder.Push(stream); err != nil {
			t.Fatal(err)
		}
	}
	for packet := 1; packet <= 255; packet++ {
		frame := frameEvent(t, time.Duration(packet), 0x18ebff80, []byte{byte(packet), 255, 255, 255, 255, 255, 255, 255})
		message, complete, err := decoder.Push(frame)
		if err != nil || complete != (packet == 255) {
			t.Fatalf("packet %d: %v, %v", packet, complete, err)
		}
		if complete && (len(message.Payload) != 1785 || message.Payload[1784] != 255) {
			t.Fatalf("maximum payload=%d bytes", len(message.Payload))
		}
	}
	for _, stream := range []gocan.FrameEvent{otherSource, otherBus, otherDirection} {
		stream.Frame.ID = 0x18ebff81
		stream.Frame.Data = [64]byte{2, 8, 9, 255, 255, 255, 255, 255}
		stream.Timestamp = stream.Timestamp.Add(256 * time.Millisecond)
		message, complete, err := decoder.Push(stream)
		if err != nil || !complete || message.Bus != stream.Bus || message.Direction != stream.Direction || !reflect.DeepEqual(message.Payload, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}) {
			t.Fatalf("interleaved: %#v, %v, %v", message, complete, err)
		}
	}
	if diagnostics := decoder.Flush(); len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
}

func TestDecoderReportsUnsupportedTraffic(t *testing.T) {
	var decoder j1939.Decoder
	for _, id := range []uint32{0x1af00480, 0x18c72180, 0x18c82180} {
		if _, complete, err := decoder.Push(frameEvent(t, 0, id, []byte{1})); complete || !errors.Is(err, j1939.ErrUnsupported) {
			t.Fatalf("%#x: %v, %v", id, complete, err)
		}
	}
	fd := frameEvent(t, 0, 0x18f00480, []byte{1})
	fd.Frame.Flags |= gocan.FrameFD
	if _, complete, err := decoder.Push(fd); complete || !errors.Is(err, j1939.ErrUnsupported) {
		t.Fatalf("CAN FD: %v, %v", complete, err)
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

	rts := frameEvent(t, 0, 0x18ec2180, []byte{16, 9, 0, 2, 2, 202, 254, 0})
	decoder.PushBatch([]gocan.FrameEvent{rts})
	abort := frameEvent(t, 2, 0x18ec8021, []byte{0xff, 3, 0, 0, 0, 0xca, 0xfe, 0x00})
	abort.Direction = gocan.DirectionTransmit
	_, diagnostics = decoder.PushBatch([]gocan.FrameEvent{abort})
	if len(diagnostics) != 1 || !errors.Is(diagnostics[0], j1939.ErrProtocol) {
		t.Fatalf("abort diagnostics = %v", diagnostics)
	}
	if remaining := decoder.Flush(); len(remaining) != 0 {
		t.Fatalf("abort left sessions: %v", remaining)
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

func ExampleDecoder() {
	database, err := dbc.Parse("engine.dbc", `BU_: ECU
BO_ 2566844032 EngineTemperature: 8 ECU
 SG_ Coolant : 0|8@1+ (1,-40) [-40|215] "degC" ECU
BA_DEF_ BO_ "VFrameFormat" ENUM "StandardCAN","ExtendedCAN","reserved","J1939PG";
BA_ "VFrameFormat" BO_ 2566844032 3;
`)
	if err != nil {
		panic(err)
	}
	definitions := database.MessagesByPGN(0xfeee)
	if len(definitions) != 1 {
		panic("select an unambiguous device definition")
	}
	capture := gocan.NewCapture() // A live Bus supplies its own Capture.
	for _, id := range []uint32{0x18feee80, 0x0cfeee81} {
		frame, err := gocan.NewFrame(id, []byte{100, 255, 255, 255, 255, 255, 255, 255}, gocan.FrameExtended)
		if err != nil {
			panic(err)
		}
		if err := capture.Append(gocan.FrameEvent{Bus: 1, Direction: gocan.DirectionReceive, Timestamp: time.Unix(1, 0), Frame: frame}); err != nil {
			panic(err)
		}
	}
	var decoder j1939.Decoder
	var cursor gocan.Cursor
	// Repeat this read as the capture grows. Retain decoder and cursor between
	// reads. Feed all raw frames before PGN filtering so TP control is observed.
	frames, next, err := capture.FramesSince(cursor)
	if errors.Is(err, gocan.ErrCursorOutOfRange) {
		decoder.Reset() // No partial payload or old NAME survives lost history.
		frames, next, err = capture.FramesSince(gocan.Cursor{})
	}
	if err != nil {
		panic(err)
	}
	cursor = next // Use this cursor for the next read and retention decisions.
	messages, diagnostics := decoder.PushBatch(frames)
	for _, diagnostic := range diagnostics {
		fmt.Println(diagnostic)
	}
	for _, message := range messages {
		if message.PGN != 0xfeee {
			continue
		}
		value, err := definitions[0].DecodePayload(message.Payload, "Coolant")
		if err != nil {
			panic(err)
		}
		fmt.Printf("source %02x: %v degC\n", message.Source, value)
	}
	for _, diagnostic := range decoder.Flush() {
		fmt.Println(diagnostic)
	} // Finite input only.
	// Raw frames remain available in capture, including unsupported traffic.
	// Output:
	// source 80: 60 degC
	// source 81: 60 degC
}
