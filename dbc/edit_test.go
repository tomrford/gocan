package dbc_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	reparsed, err := dbc.Parse("device-A-source.dbc", deviceA.Source())
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
	fdSource, err := dbc.Parse("device-A-fd-source.dbc", fd.Source())
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
		want string
	}{
		"colliding identifiers":      {func(db *dbc.Database) { db.Messages[1].ID = db.Messages[0].ID }, "duplicate message ID"},
		"identifier beyond 29 bits":  {func(db *dbc.Database) { db.Messages[0].ID |= 0xF << 28 }, "29-bit"},
		"extended identifier bit 31": {func(db *dbc.Database) { db.Messages[0].ID |= 1 << 31 }, "29-bit"},
		"standard identifier bit 31": {func(db *dbc.Database) {
			db.Messages[0].Extended = false
			db.Messages[0].ID = 0x80000001
		}, "11-bit"},
		"name with a space":             {func(db *dbc.Database) { db.Messages[0].Name = "DEV 0 STAT" }, `"DEV 0 STAT"`},
		"comment ending in a backslash": {func(db *dbc.Database) { db.Comment = `C:\logs\` }, `C:\\logs\\`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := base.Edit("invalid.dbc", test.edit); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Edit() error = %v, want it to mention %s", err, test.want)
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
