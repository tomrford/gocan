package dbc

import (
	"math"
	"strings"
	"testing"

	"github.com/tomrford/gocan"
)

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
	assertDecoded(t, command, frame, "Mode", uint64(1))
	assertDecoded(t, command, frame, "Temperature", 25.5)
	assertDecoded(t, command, frame, "BigEndian", uint64(0xabcd))
	assertDecoded(t, command, frame, "SignedCounter", int64(-2))

	if err := command.Patch(&frame, Values{"Temperature": 50.0}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	assertDecoded(t, command, frame, "Temperature", 50.0)

	if _, err := command.Encode(Values{"Enable": true}); err == nil || !strings.Contains(err.Error(), "requires signal") {
		t.Fatalf("incomplete Encode error = %v", err)
	}
	beforeInvalidPatch := frame
	if err := command.Patch(&frame, Values{"Mode": "Unknown"}); err == nil || !strings.Contains(err.Error(), "unknown value description") {
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
