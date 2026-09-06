package dbc

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/tomrford/gocan"
)

func BenchmarkMessageCodecCompilation(b *testing.B) {
	for _, test := range []struct {
		name   string
		length uint32
		count  int
		width  uint32
	}{
		{"classic", 8, 8, 8},
		{"FD", 64, 32, 16},
		{"transported", 1785, 128, 64},
		{"sparse", math.MaxUint32, 2, 64},
	} {
		b.Run(test.name, func(b *testing.B) {
			message := Message{Length: test.length}
			for index := range test.count {
				message.Signals = append(message.Signals, Signal{
					Name: fmt.Sprintf("Signal%d", index), StartBit: uint32(index) * test.width,
					BitLength: test.width, ByteOrder: ByteOrderLittleEndian, Factor: 1,
				})
			}
			b.ReportAllocs()
			for b.Loop() {
				codec := compileMessageCodec(&message)
				if codec.err != nil || codec.writeErr != nil {
					b.Fatalf("compile: %v, %v", codec.err, codec.writeErr)
				}
			}
		})
	}
}

func TestMessageOverlapWordBoundaries(t *testing.T) {
	for _, test := range []struct {
		name  string
		start uint32
		order ByteOrder
		bytes [16]byte
	}{
		// Literal occupied payload bits for unaligned 64-bit signals.
		{"Intel", 63, ByteOrderLittleEndian, [16]byte{7: 0x80, 8: 0xff, 9: 0xff, 10: 0xff, 11: 0xff, 12: 0xff, 13: 0xff, 14: 0xff, 15: 0x7f}},
		{"Motorola", 56, ByteOrderBigEndian, [16]byte{7: 0x01, 8: 0xff, 9: 0xff, 10: 0xff, 11: 0xff, 12: 0xff, 13: 0xff, 14: 0xff, 15: 0xfe}},
	} {
		for _, offset := range []uint32{0, 512, math.MaxUint32 - 255} {
			t.Run(fmt.Sprintf("%s/%d", test.name, offset), func(t *testing.T) {
				for bit := uint32(0); bit < 192; bit++ {
					wantOverlap := bit < 128 && test.bytes[bit/8]&(1<<(bit%8)) != 0
					message := Message{Length: math.MaxUint32, Signals: []Signal{
						{Name: "Wide", StartBit: offset + test.start, BitLength: 64, ByteOrder: test.order, Factor: 1},
						{Name: "Probe", StartBit: offset + bit, BitLength: 1, ByteOrder: ByteOrderLittleEndian, Factor: 1},
					}}
					for range 2 {
						codec := compileMessageCodec(&message)
						if codec.err != nil || (codec.writeErr != nil) != wantOverlap {
							t.Fatalf("bit %d: compile = %v, %v; want overlap %t", bit, codec.err, codec.writeErr, wantOverlap)
						}
						message.Signals[0], message.Signals[1] = message.Signals[1], message.Signals[0]
					}
				}
			})
		}
	}
}

func TestMessageCodecLifecycle(t *testing.T) {
	db := parseFixture(t, "testdata/codec.dbc")
	command, ok := db.MessageByName("Command")
	if !ok {
		t.Fatal("Command message was not resolved")
	}

	frame, err := command.Encode(Values{
		"Enable":        true,
		"Mode":          "Torque",
		"Temperature":   25.5,
		"BigEndian":     uint64(0xabcd),
		"SignedCounter": int64(-2),
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	wantData := [gocan.MaxDataLength]byte{0x03, 0x8f, 0x02, 0xab, 0xcd, 0xfe, 0x00, 0x00}
	if frame.ID != 0x123 || frame.DLC != 8 || frame.Flags != 0 || frame.Data != wantData {
		t.Fatalf("encoded frame = %#v, want ID 0x123 and data % x", frame, wantData[:8])
	}

	assertDecoded(t, command, frame, "Enable", uint64(1))
	assertDecoded(t, command, frame, "Mode", "Torque")
	assertDecoded(t, command, frame, "Temperature", 25.5)
	assertDecoded(t, command, frame, "BigEndian", uint64(0xabcd))
	assertDecoded(t, command, frame, "SignedCounter", int64(-2))

	// Classical DLC values 9 through 15 still describe eight data bytes.
	frame.DLC = 15
	if err := command.Patch(&frame, Values{"Temperature": 50.0}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	assertDecoded(t, command, frame, "Temperature", 50.0)

	// An off-grid physical value is quantized to the nearest raw value.
	if err := command.Patch(&frame, Values{"Temperature": 50.03}); err != nil {
		t.Fatalf("Patch off-grid Temperature: %v", err)
	}
	assertDecoded(t, command, frame, "Temperature", 50.0)

	// A value description encodes even when its raw value lies outside the
	// physical range, matching the not-available idiom.
	if err := command.Patch(&frame, Values{"SignedCounter": "SNA"}); err != nil {
		t.Fatalf("Patch SNA: %v", err)
	}
	assertDecoded(t, command, frame, "SignedCounter", "SNA")

	if _, err := command.Encode(Values{"Enable": true}); err == nil || !strings.Contains(err.Error(), "requires signal") {
		t.Fatalf("incomplete Encode error = %v", err)
	}
	beforeInvalidPatch := frame
	if err := command.Patch(&frame, Values{"Mode": "Unknown"}); err == nil || !strings.Contains(err.Error(), "unknown label") {
		t.Fatalf("unknown value description error = %v", err)
	}
	if err := command.Patch(&frame, Values{"Temperature": 300.0}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("out-of-range Patch error = %v", err)
	}
	if frame != beforeInvalidPatch {
		t.Fatal("failed Patch modified frame")
	}

	floatStatus, ok := db.MessageByName("FloatStatus")
	if !ok {
		t.Fatal("FloatStatus message was not resolved")
	}
	floatFrame, err := floatStatus.Encode(Values{"Ratio": 0.25})
	if err != nil {
		t.Fatalf("Encode FloatStatus: %v", err)
	}
	assertDecoded(t, floatStatus, floatFrame, "Ratio", 0.25)

	fast, ok := db.MessageByName("FastStatus")
	if !ok {
		t.Fatal("FastStatus message was not resolved")
	}
	const payload = uint64(0xfedcba9876543210)
	fd, err := fast.Encode(Values{"Payload": payload, "Tail": uint64(0x5a)})
	if err != nil {
		t.Fatalf("Encode FastStatus: %v", err)
	}
	wantFlags := gocan.FrameExtended | gocan.FrameFD | gocan.FrameBitRateSwitch
	if fd.Flags != wantFlags || fd.DLC != 9 || fd.DataLength() != 12 || fd.Data[8] != 0x5a {
		t.Fatalf("encoded CAN FD frame = %#v", fd)
	}
	assertDecoded(t, fast, fd, "Payload", payload)

	wide, ok := db.MessageByName("WideData")
	if !ok {
		t.Fatal("WideData message was not resolved")
	}
	if _, err := wide.Encode(Values{"Payload": uint64(0)}); err == nil || !strings.Contains(err.Error(), "exceeds the 64-bit codec representation") {
		t.Fatalf("wide signal Encode error = %v", err)
	}
}

func TestMultiplexedPatchAndJ1939(t *testing.T) {
	db := parseFixture(t, "testdata/multiplex_j1939.dbc")
	multiplexed, ok := db.MessageByName("NestedMux")
	if !ok {
		t.Fatal("NestedMux message was not resolved")
	}
	frame, err := multiplexed.Encode(Values{
		"RootA":         uint64(2),
		"ChildSelector": uint64(3),
		"Leaf":          uint64(44),
		"RootB":         uint64(7),
		"Other":         uint64(55),
	})
	if err != nil {
		t.Fatalf("Encode NestedMux: %v", err)
	}
	assertDecoded(t, multiplexed, frame, "Leaf", uint64(44))
	assertDecoded(t, multiplexed, frame, "Other", uint64(55))

	inactive := frame
	if err := multiplexed.Patch(&inactive, Values{"RootA": uint64(1)}); err != nil {
		t.Fatalf("Patch to inactive branch: %v", err)
	}
	if _, err := multiplexed.Decode(inactive, "Leaf"); err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("inactive Decode error = %v", err)
	}
	if err := multiplexed.Patch(&inactive, Values{"RootA": uint64(2)}); err == nil || !strings.Contains(err.Error(), "requires a value") {
		t.Fatalf("incomplete path change error = %v", err)
	}
	reactivated := inactive
	err = multiplexed.Patch(&reactivated, Values{
		"RootA":         uint64(2),
		"ChildSelector": uint64(4),
		"Leaf":          uint64(99),
	})
	if err != nil {
		t.Fatalf("Patch to active branch: %v", err)
	}
	assertDecoded(t, multiplexed, reactivated, "Leaf", uint64(99))
	assertDecoded(t, multiplexed, reactivated, "Other", uint64(55))

	j1939, ok := db.MessageByName("EngineTemperature")
	if !ok {
		t.Fatal("EngineTemperature message was not resolved")
	}
	j1939Frame, err := j1939.Encode(Values{"Coolant": 60.0})
	if err != nil {
		t.Fatalf("Encode EngineTemperature: %v", err)
	}
	if j1939Frame.ID != 0x18feee80 || !j1939Frame.Flags.Has(gocan.FrameExtended) {
		t.Fatalf("encoded J1939 frame = %#v", j1939Frame)
	}
	assertDecoded(t, j1939, j1939Frame, "Coolant", 60.0)
	// The selected PGN definition applies across priority/source changes.
	j1939Frame.ID = 0x0cfeee21
	assertDecoded(t, j1939, j1939Frame, "Coolant", 60.0)
	if err := j1939.Patch(&j1939Frame, Values{"Coolant": 70.0}); err != nil {
		t.Fatalf("Patch another source: %v", err)
	}
	assertDecoded(t, j1939, j1939Frame, "Coolant", 70.0)
	j1939Frame.ID = 0x0cfeef21
	if _, err := j1939.Decode(j1939Frame, "Coolant"); err == nil {
		t.Fatal("decoded another PGN")
	}
}

func TestClassicIgnoresCANFDBRSDefault(t *testing.T) {
	source := "BU_: ECU\n" +
		"BO_ 256 Classic: 8 ECU\n" +
		" SG_ Value : 0|8@1+ (1,0) [0|255] \"\" ECU\n" +
		"BA_DEF_ BO_ \"CANFD_BRS\" ENUM \"0\",\"1\";\n" +
		"BA_DEF_DEF_ \"CANFD_BRS\" \"1\";\n"

	database, err := Parse("real-world.dbc", source)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	classic, ok := database.MessageByName("Classic")
	if !ok {
		t.Fatal("Classic message was not resolved")
	}
	frame, err := classic.Encode(Values{"Value": uint64(7)})
	if err != nil {
		t.Fatalf("Encode Classic: %v", err)
	}
	if frame.Flags != 0 {
		t.Fatalf("Classic flags = %#x, want no CAN FD or BRS flags", frame.Flags)
	}
	assertDecoded(t, classic, frame, "Value", uint64(7))
}

func TestMessagesByPGNCanonicalisesPDU1Identifiers(t *testing.T) {
	const source = "BU_: ECU\n" +
		"BO_ 2564432256 First: 8 ECU\n" +
		" SG_ Value : 0|8@1+ (1,0) [0|255] \"\" ECU\n" +
		"BO_ 2564432513 Second: 8 ECU\n" +
		" SG_ Value : 0|8@1+ (1,0) [0|255] \"\" ECU\n" +
		"BA_DEF_ BO_ \"VFrameFormat\" ENUM \"StandardCAN\",\"ExtendedCAN\",\"reserved\",\"J1939PG\";\n" +
		"BA_ \"VFrameFormat\" BO_ 2564432256 3;\n" +
		"BA_ \"VFrameFormat\" BO_ 2564432513 3;\n"
	database, err := Parse("pdu1.dbc", source)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	matches := database.MessagesByPGN(0xda00)
	if len(matches) != 2 || matches[0].Name != "First" || matches[1].Name != "Second" {
		t.Fatalf("MessagesByPGN = %#v", matches)
	}
}

func TestLongJ1939PayloadDecode(t *testing.T) {
	source := "BU_: ECU\n" +
		"BO_ 2566834942 LongJ1939: 1785 ECU\n" +
		" SG_ DTC : 16|32@1+ (1,0) [0|4294967295] \"\" ECU\n" +
		" SG_ HighSignal : 1544|64@1+ (1,0) [0|4294967295] \"\" ECU\n" +
		"BA_DEF_ BO_ \"VFrameFormat\" ENUM \"StandardCAN\",\"ExtendedCAN\",\"reserved\",\"J1939PG\";\n" +
		"BA_ \"VFrameFormat\" BO_ 2566834942 3;\n"

	database, err := Parse("long-j1939.dbc", source)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	long, ok := database.MessageByName("LongJ1939")
	if !ok {
		t.Fatal("LongJ1939 message was not resolved")
	}
	if long.Format != FrameFormatJ1939 || long.Length != 1785 || len(long.Signals) != 2 {
		t.Fatalf("LongJ1939 metadata = format %d length %d signals %d", long.Format, long.Length, len(long.Signals))
	}
	if _, ok := long.SignalByName("DTC"); !ok {
		t.Fatal("DTC signal was not resolved")
	}
	payload := make([]byte, 201)
	payload[2] = 0x78
	payload[3] = 0x56
	payload[4] = 0x34
	payload[5] = 0x12
	payload[193] = 0x10
	payload[194] = 0x32
	payload[195] = 0x54
	payload[196] = 0x76
	payload[197] = 0x98
	payload[198] = 0xba
	payload[199] = 0xdc
	payload[200] = 0xfe
	if value, err := long.DecodePayload(payload, "DTC"); err != nil || value != uint64(0x12345678) {
		t.Fatalf("DecodePayload DTC = %#v, %v", value, err)
	}
	if value, err := long.DecodePayload(payload, "HighSignal"); err != nil || value != uint64(0xfedcba9876543210) {
		t.Fatalf("DecodePayload HighSignal = %#v, %v", value, err)
	}
	if _, err := long.DecodePayload(payload[:100], "HighSignal"); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("short DecodePayload error = %v", err)
	}
	if _, err := long.DecodePayload(make([]byte, 1786), "DTC"); err == nil || !strings.Contains(err.Error(), "exceeds declared length") {
		t.Fatalf("long DecodePayload error = %v", err)
	}
	if _, err := long.Encode(Values{"DTC": uint64(1)}); err == nil || !strings.Contains(err.Error(), "exceeds one classical CAN frame") {
		t.Fatalf("LongJ1939 Encode error = %v", err)
	}
	if _, err := long.Decode(gocan.Frame{}, "DTC"); err == nil || !strings.Contains(err.Error(), "exceeds one classical CAN frame") {
		t.Fatalf("LongJ1939 Decode error = %v", err)
	}
	frame := gocan.Frame{}
	if err := long.Patch(&frame, Values{"DTC": uint64(1)}); err == nil || !strings.Contains(err.Error(), "exceeds one classical CAN frame") {
		t.Fatalf("LongJ1939 Patch error = %v", err)
	}
}

func TestEncodePayloadFrameFormats(t *testing.T) {
	db := parseFixture(t, "testdata/codec.dbc")
	for _, test := range []struct {
		name   string
		values Values
		want   []byte
	}{
		{"Command", Values{"Enable": true, "Mode": "Torque", "Temperature": 25.5, "BigEndian": uint64(0xabcd), "SignedCounter": int64(-2)},
			[]byte{0x03, 0x8f, 0x02, 0xab, 0xcd, 0xfe, 0, 0}},
		{"FastStatus", Values{"Payload": uint64(0xfedcba9876543210), "Tail": uint64(0x5a)},
			[]byte{0x10, 0x32, 0x54, 0x76, 0x98, 0xba, 0xdc, 0xfe, 0x5a}},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, _ := db.MessageByName(test.name)
			payload, err := message.EncodePayload(test.values)
			if err != nil || !bytes.Equal(payload, test.want) {
				t.Fatalf("EncodePayload = % x, %v; want % x", payload, err, test.want)
			}
		})
	}
	wide, _ := db.MessageByName("WideData")
	if payload, err := wide.EncodePayload(Values{"Payload": uint64(0)}); payload != nil || err == nil || !strings.Contains(err.Error(), "exceeds the 64-bit codec representation") {
		t.Fatalf("wide signal EncodePayload = % x, %v", payload, err)
	}
}

func TestEncodePayloadTransported(t *testing.T) {
	// The payload codec has no J1939 TP size limit. The caller selects a
	// transport capable of carrying this message's declared length.
	const source = `BU_: ECU
BO_ 2566834942 Long: 1800 ECU
 SG_ Selector M : 568|8@1+ (1,0) [0|255] "" ECU
 SG_ Intel m1 : 63|16@1+ (1,0) [0|65535] "" ECU
 SG_ Motorola m1 : 519|16@0- (0.5,-10) [-100|100] "" ECU
 SG_ Tail m2 : 14392|8@1+ (1,0) [0|255] "" ECU
BA_DEF_ BO_ "VFrameFormat" ENUM "StandardCAN","ExtendedCAN","reserved","J1939PG";
BA_ "VFrameFormat" BO_ 2566834942 3;
`
	db, err := Parse("transported.dbc", source)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := db.MessageByName("Long")
	values := Values{"Selector": uint64(1), "Intel": uint64(0xabcd), "Motorola": -11.0}
	payload, err := message.EncodePayload(values)
	want := make([]byte, 1800)
	// 0xabcd begins at bit 7 of byte 7. -11 encodes as signed raw -2.
	want[7], want[8], want[9] = 0x80, 0xe6, 0x55
	want[64], want[65], want[71] = 0xff, 0xfe, 1
	if err != nil || !bytes.Equal(payload, want) {
		t.Fatalf("EncodePayload = % x, %v; want % x", payload, err, want)
	}
	for name, expected := range values {
		if got, err := message.DecodePayload(payload, name); err != nil || got != expected {
			t.Fatalf("DecodePayload %s = %v, %v; want %v", name, got, err, expected)
		}
	}
	other, err := message.EncodePayload(Values{"Selector": uint64(2), "Tail": uint64(0xa5)})
	wantOther := make([]byte, 1800)
	wantOther[71], wantOther[1799] = 2, 0xa5
	if err != nil || !bytes.Equal(other, wantOther) || !bytes.Equal(payload, want) {
		t.Fatalf("second encoding changed the first payload or encoded the wrong branch: %v", err)
	}
	for _, test := range []struct {
		values Values
		error  string
	}{
		{Values{"Selector": uint64(1), "Intel": uint64(1)}, "requires signal"},
		{Values{"Selector": uint64(2), "Tail": uint64(1), "Intel": uint64(1)}, "inactive"},
		{Values{"Unknown": uint64(1)}, "has no signal"},
		{Values{"Selector": uint64(2), "Tail": uint64(256)}, "outside"},
		{Values{"Intel": uint64(1)}, "requires multiplexor"},
	} {
		if got, err := message.EncodePayload(test.values); got != nil || err == nil || !strings.Contains(err.Error(), test.error) {
			t.Fatalf("EncodePayload(%v) = % x, %v; want nil and %q", test.values, got, err, test.error)
		}
	}
	if _, err := message.Encode(values); err == nil || !strings.Contains(err.Error(), "exceeds one classical CAN frame") {
		t.Fatalf("Encode transported message error = %v", err)
	}
}

func assertDecoded(t *testing.T, message *Message, frame gocan.Frame, signal string, want any) {
	t.Helper()
	got, err := message.Decode(frame, signal)
	if err != nil {
		t.Fatalf("Decode %s: %v", signal, err)
	}
	gotFloat, gotIsFloat := got.(float64)
	wantFloat, wantIsFloat := want.(float64)
	if gotIsFloat && wantIsFloat && math.Abs(gotFloat-wantFloat) <= 1e-9 {
		return
	}
	if got != want {
		t.Fatalf("Decode %s = %#v, want %#v", signal, got, want)
	}
}
