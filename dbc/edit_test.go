package dbc_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/dbc"
	"golang.org/x/text/encoding/charmap"
)

// The device fixture describes device ID 0, whose ID occupies identifier bits
// 8-11, the way a single-device interface DBC is shipped and then derived for
// each installed device.
func TestEditDerivesDeviceDatabases(t *testing.T) {
	base, err := dbc.ParseFile("testdata/device.dbc")
	if err != nil {
		t.Fatal(err)
	}
	forDevice := func(device uint32) func(*dbc.Message) {
		return func(message *dbc.Message) {
			message.ID |= device << 8
			message.Name = strings.Replace(message.Name, "_0_", fmt.Sprintf("_%X_", device), 1)
		}
	}

	deviceA, err := base.Edit("device-A.dbc", func(db *dbc.Database) {
		for index := range db.Messages {
			forDevice(0xA)(&db.Messages[index])
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDevice := func(t *testing.T, db *dbc.Database, device uint32) {
		t.Helper()
		name := fmt.Sprintf("DEV_%X_STAT", device)
		status, ok := db.MessageByID(0x10061003|device<<8, true)
		if !ok || status.Name != name || status.Comment != "Device status" ||
			status.Attributes["GenMsgCycleTime"].Integer != 100 {
			t.Fatalf("device %X status message: %#v", device, status)
		}
		coolant, _ := status.SignalByName("Coolant")
		state, _ := status.SignalByName("State")
		if coolant == nil || coolant.Comment != "Kühlmittel inlet" || coolant.Unit != "°C" ||
			state == nil || len(state.Values) != 3 || state.Values[2].Label != "Fault" {
			t.Fatalf("device %X status signals: %#v", device, status.Signals)
		}
		control, ok := db.MessageByName(fmt.Sprintf("DEV_%X_CTRL", device))
		if !ok {
			t.Fatalf("device %X control message is missing", device)
		}
		frame, err := control.Encode(dbc.Values{"Enable": true, "Setpoint": 12.5})
		if err != nil {
			t.Fatal(err)
		}
		if frame.ID != 0x10063001|device<<8 || !frame.Flags.Has(gocan.FrameExtended) || frame.Data[0] != 1 || frame.Data[1] != 25 {
			t.Fatalf("device %X control frame: %v", device, frame)
		}
	}
	assertDevice(t, deviceA, 0xA)
	if _, ok := deviceA.MessageByName("DEV_0_STAT"); ok {
		t.Fatal("derived database still answers the device 0 name")
	}
	// The derived source is the DBC to store alongside device A's traces.
	reparsed, err := dbc.Parse("device-A-source.dbc", bytes.NewReader(deviceA.Source()))
	if err != nil {
		t.Fatal(err)
	}
	assertDevice(t, reparsed, 0xA)

	// Frame format is edited through the DBC attributes that declare it.
	fd, err := deviceA.Edit("device-A-fd.dbc", func(db *dbc.Database) {
		db.AttributeDefinitions = append(db.AttributeDefinitions, dbc.AttributeDefinition{
			Name: "VFrameFormat", Scope: dbc.AttributeScopeMessage, Kind: dbc.AttributeKindInteger, Maximum: 15,
		})
		db.Messages[1].Attributes["VFrameFormat"] = dbc.AttributeValue{Kind: dbc.AttributeKindInteger, Integer: 15}
	})
	if err != nil {
		t.Fatal(err)
	}
	fdSource, err := dbc.Parse("device-A-fd-source.dbc", bytes.NewReader(fd.Source()))
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range []*dbc.Database{fd, fdSource} {
		assertDevice(t, db, 0xA)
		control, _ := db.MessageByName("DEV_A_CTRL")
		frame, err := control.Encode(dbc.Values{"Enable": true, "Setpoint": 12.5})
		if err != nil || control.Format != dbc.FrameFormatExtendedCANFD || !frame.Flags.Has(gocan.FrameFD) {
			t.Fatalf("edited CAN FD control: format=%v frame=%v error=%v", control.Format, frame, err)
		}
	}
	if _, changed := deviceA.Messages[1].Attributes["VFrameFormat"]; changed || deviceA.Messages[1].Format != dbc.FrameFormatExtendedCAN {
		t.Fatal("frame-format edit changed the original database")
	}

	// One database for every device, as a release would ship it.
	everyDevice, err := base.Edit("device-all.dbc", func(db *dbc.Database) {
		device0 := db.Messages
		db.Messages = nil
		for device := range uint32(16) {
			for _, message := range device0 {
				forDevice(device)(&message)
				db.Messages = append(db.Messages, message)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(everyDevice.Messages) != 48 {
		t.Fatalf("expanded database has %d messages, want 48", len(everyDevice.Messages))
	}
	for _, device := range []uint32{0, 7, 0xF} {
		assertDevice(t, everyDevice, device)
	}

	// Windows-1252 for tools that expect it; the file is the test's artifact.
	path := filepath.Join(t.TempDir(), "device-A.dbc")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := deviceA.Write(charmap.Windows1252.NewEncoder().Writer(file)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(written, []byte("K\xfchlmittel")) {
		t.Fatal("written file is not Windows-1252")
	}
	fromFile, err := dbc.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertDevice(t, fromFile, 0xA)

	for name, test := range map[string]struct {
		edit func(*dbc.Database)
		// keyword is the record Parse rejects; otherwise Write rejects the
		// edited model as unrepresentable.
		keyword string
	}{
		"colliding identifiers":      {func(db *dbc.Database) { db.Messages[1].ID = db.Messages[0].ID }, "BO_"},
		"identifier beyond 29 bits":  {func(db *dbc.Database) { db.Messages[0].ID |= 0xF << 28 }, ""},
		"extended identifier bit 31": {func(db *dbc.Database) { db.Messages[0].ID |= 1 << 31 }, ""},
		"standard identifier bit 31": {func(db *dbc.Database) {
			db.Messages[0].Extended = false
			db.Messages[0].ID = 0x80000001
		}, ""},
		"name with a space":             {func(db *dbc.Database) { db.Messages[0].Name = "DEV 0 STAT" }, ""},
		"comment ending in a backslash": {func(db *dbc.Database) { db.Comment = `C:\logs\` }, ""},
		"nil byte order":                {func(db *dbc.Database) { db.Messages[0].Signals[0].ByteOrder = nil }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			edited, err := base.Edit("invalid.dbc", test.edit)
			var parseError *dbc.Error
			switch {
			case edited != nil:
				t.Fatalf("Edit() = %v, want an error", edited)
			case test.keyword == "" && !errors.Is(err, dbc.ErrUnrepresentable):
				t.Fatalf("Edit() error = %v, want %v", err, dbc.ErrUnrepresentable)
			case test.keyword != "" && (!errors.As(err, &parseError) || parseError.Keyword != test.keyword):
				t.Fatalf("Edit() error = %v, want a %s parse error", err, test.keyword)
			}
		})
	}

	// Every edit worked on a copy.
	status, ok := base.MessageByID(0x10061003, true)
	if !ok || status.Name != "DEV_0_STAT" || len(base.Messages) != 3 || base.Messages[1].ID != 0x10063001 ||
		base.Comment == `C:\logs\` {
		t.Fatalf("base database changed: %#v", base.Messages)
	}
}

// A database built in code rather than parsed is edited like a parsed one: the
// callback's lookups find what db declares.
func TestEditConstructedDatabase(t *testing.T) {
	constructed := &dbc.Database{
		Nodes: []dbc.Node{{Name: "ECU"}},
		Messages: []dbc.Message{{
			ID: 0x100, Name: "Status", Length: 1, Transmitters: []string{"ECU"},
			Signals: []dbc.Signal{{Name: "Value", BitLength: 8, ByteOrder: binary.LittleEndian, Factor: 1, Maximum: 255}},
		}},
	}
	edited, err := constructed.Edit("constructed.dbc", func(db *dbc.Database) {
		status, ok := db.MessageByName("Status")
		if !ok {
			t.Fatal("callback lookup missed Status")
		}
		value, ok := status.SignalByName("Value")
		if !ok {
			t.Fatal("callback lookup missed Value")
		}
		value.Name = "Level"
		// The renamed signal is found by its new name for further edits.
		level, ok := status.SignalByName("Level")
		if !ok {
			t.Fatal("callback lookup missed the renamed signal")
		}
		level.Comment = "Fill level (Ω)"
	})
	if err != nil {
		t.Fatal(err)
	}
	if constructed.Source() != nil {
		t.Fatal("constructed database unexpectedly has Source bytes")
	}
	if src := edited.Source(); bytes.HasPrefix(src, []byte{0xef, 0xbb, 0xbf}) || !utf8.Valid(src) || !bytes.Contains(src, []byte("Fill level (Ω)")) {
		t.Fatalf("constructed Edit source is not plain UTF-8: %q", src)
	}
	status, ok := edited.MessageByID(0x100, false)
	if !ok {
		t.Fatal("edited database lost Status")
	}
	frame, err := status.Encode(dbc.Values{"Level": uint64(42)})
	if err != nil || frame.Data[0] != 42 {
		t.Fatalf("Encode edited Status = %v, %v", frame, err)
	}
	if level, ok := status.SignalByName("Level"); !ok || level.Comment != "Fill level (Ω)" {
		t.Fatalf("edited Level = %#v", level)
	}
	if constructed.Messages[0].Signals[0].Name != "Value" {
		t.Fatal("Edit changed the constructed database")
	}
}

func TestEditPreservesSourceEncoding(t *testing.T) {
	utf8BOM := []byte{0xef, 0xbb, 0xbf}
	plainUTF8, err := os.ReadFile("testdata/device.dbc")
	if err != nil {
		t.Fatal(err)
	}
	win1252, err := charmap.Windows1252.NewEncoder().Bytes(plainUTF8)
	if err != nil {
		t.Fatal(err)
	}
	withBOM := append(append([]byte(nil), utf8BOM...), plainUTF8...)

	t.Run("Windows-1252", func(t *testing.T) {
		base, err := dbc.Parse("device-1252.dbc", bytes.NewReader(win1252))
		if err != nil {
			t.Fatal(err)
		}
		edited, err := base.Edit("device-A-1252.dbc", func(db *dbc.Database) {
			db.Messages[0].ID |= 0xA << 8
			db.Messages[0].Name = "DEV_A_STAT"
			db.Comment = "Kosten: 5€"
		})
		if err != nil {
			t.Fatal(err)
		}
		src := edited.Source()
		if utf8.Valid(src) || bytes.HasPrefix(src, utf8BOM) {
			t.Fatalf("Windows-1252 Edit produced UTF-8 or BOM source: %q", src)
		}
		for _, want := range [][]byte{[]byte("K\xfchlmittel inlet"), []byte("\xb0C"), []byte("Kosten: 5\x80")} {
			if !bytes.Contains(src, want) {
				t.Fatalf("Windows-1252 Edit source missing %x", want)
			}
		}
		reparsed, err := dbc.Parse("reparsed-1252.dbc", bytes.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		for _, db := range []*dbc.Database{edited, reparsed} {
			status, ok := db.MessageByName("DEV_A_STAT")
			if !ok {
				t.Fatal("DEV_A_STAT missing")
			}
			coolant, _ := status.SignalByName("Coolant")
			if db.Comment != "Kosten: 5€" || coolant == nil || coolant.Comment != "Kühlmittel inlet" || coolant.Unit != "°C" {
				t.Fatalf("unexpected decoded Windows-1252 values: comment=%q coolant=%#v", db.Comment, coolant)
			}
		}

		// Write still produces UTF-8 regardless of the source encoding.
		var written bytes.Buffer
		if err := edited.Write(&written); err != nil {
			t.Fatal(err)
		}
		if bytes.HasPrefix(written.Bytes(), utf8BOM) || !utf8.Valid(written.Bytes()) ||
			!bytes.Contains(written.Bytes(), []byte("Kühlmittel inlet")) ||
			!bytes.Contains(written.Bytes(), []byte("Kosten: 5€")) {
			t.Fatalf("Write on Windows-1252 database did not write plain UTF-8: %q", written.Bytes())
		}

		// Chained edits keep Windows-1252 even across an ASCII-only intermediate edit.
		asciiOnly, err := edited.Edit("ascii-step.dbc", func(db *dbc.Database) {
			db.Comment = "ASCII only"
			coolant, _ := db.Messages[0].SignalByName("Coolant")
			coolant.Comment = "Coolant inlet"
			coolant.Unit = "C"
		})
		if err != nil {
			t.Fatal(err)
		}
		restored, err := asciiOnly.Edit("restored-1252.dbc", func(db *dbc.Database) {
			db.Comment = "Rücklauf °C"
		})
		if err != nil {
			t.Fatal(err)
		}
		if src := restored.Source(); utf8.Valid(src) || !bytes.Contains(src, []byte("R\xfccklauf \xb0C")) {
			t.Fatalf("chained Windows-1252 Edit lost source encoding: %q", src)
		}
		if restored.Comment != "Rücklauf °C" {
			t.Fatalf("restored comment = %q, want %q", restored.Comment, "Rücklauf °C")
		}

		// Windows-1252 bytes that also happen to be valid UTF-8 (e.g. "Ã©" -> C3 A9)
		// must not be reinterpreted as UTF-8 in the returned model.
		utf8Like, err := asciiOnly.Edit("utf8-like-1252.dbc", func(db *dbc.Database) {
			db.Comment = "Ã©"
		})
		if err != nil {
			t.Fatal(err)
		}
		if utf8Like.Comment != "Ã©" || !bytes.Contains(utf8Like.Source(), []byte("CM_ \"\xc3\xa9\";")) {
			t.Fatalf("Windows-1252 UTF-8-like bytes misread: comment=%q source=%q", utf8Like.Comment, utf8Like.Source())
		}

		// Characters outside Windows-1252 fail with ErrUnrepresentable without changing base.
		bad, err := base.Edit("bad-1252.dbc", func(db *dbc.Database) {
			db.Comment = "Resistance in Ω"
		})
		if bad != nil || !errors.Is(err, dbc.ErrUnrepresentable) {
			t.Fatalf("Edit with non-Windows-1252 character = %v, %v; want nil, %v", bad, err, dbc.ErrUnrepresentable)
		}
		if !bytes.Equal(base.Source(), win1252) {
			t.Fatal("failed Windows-1252 Edit modified original source")
		}
	})

	t.Run("UTF-8 with BOM", func(t *testing.T) {
		base, err := dbc.Parse("device-bom.dbc", bytes.NewReader(withBOM))
		if err != nil {
			t.Fatal(err)
		}
		edited, err := base.Edit("device-A-bom.dbc", func(db *dbc.Database) {
			db.Comment = "Resistance in Ω"
		})
		if err != nil {
			t.Fatal(err)
		}
		src := edited.Source()
		if !bytes.HasPrefix(src, utf8BOM) || !utf8.Valid(src) ||
			!bytes.Contains(src, []byte("Resistance in Ω")) ||
			!bytes.Contains(src, []byte("Kühlmittel inlet")) {
			t.Fatalf("UTF-8 BOM Edit source did not preserve BOM and UTF-8 text: %q", src)
		}
		reparsed, err := dbc.Parse("reparsed-bom.dbc", bytes.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		if edited.Comment != "Resistance in Ω" || reparsed.Comment != "Resistance in Ω" {
			t.Fatalf("unexpected comments: edited=%q reparsed=%q", edited.Comment, reparsed.Comment)
		}

		// Chained edit and whole-struct replacement both preserve the BOM;
		// Write stays BOM-free UTF-8.
		chained, err := edited.Edit("device-B-bom.dbc", func(db *dbc.Database) {
			*db = dbc.Database{Version: "2.0", Comment: "Replaced"}
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(chained.Source(), utf8BOM) || chained.Comment != "Replaced" {
			t.Fatalf("chained Edit dropped UTF-8 BOM or comment: comment=%q source=%q", chained.Comment, chained.Source())
		}
		var written bytes.Buffer
		if err := edited.Write(&written); err != nil {
			t.Fatal(err)
		}
		if bytes.HasPrefix(written.Bytes(), utf8BOM) || !utf8.Valid(written.Bytes()) {
			t.Fatalf("Write on BOM database included BOM or invalid UTF-8: %q", written.Bytes())
		}
	})

	t.Run("plain UTF-8", func(t *testing.T) {
		base, err := dbc.Parse("device-utf8.dbc", bytes.NewReader(plainUTF8))
		if err != nil {
			t.Fatal(err)
		}
		edited, err := base.Edit("device-A-utf8.dbc", func(db *dbc.Database) {
			db.Comment = "Resistance in Ω"
		})
		if err != nil {
			t.Fatal(err)
		}
		src := edited.Source()
		if bytes.HasPrefix(src, utf8BOM) || !utf8.Valid(src) ||
			!bytes.Contains(src, []byte("Resistance in Ω")) ||
			!bytes.Contains(src, []byte("Kühlmittel inlet")) {
			t.Fatalf("plain UTF-8 Edit source unexpected: %q", src)
		}
	})
}
