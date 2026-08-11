package cdd_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomrford/gocan/cdd"
)

// TestParseVectorDocument fixes the resolution required by real Vector input:
// the identifier width and record layout both arrive through references.
func TestParseVectorDocument(t *testing.T) {
	database, err := cdd.ParseFile(filepath.Join("testdata", "vector-diddataref.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	if len(database.Diagnostics) != 0 || len(database.DIDs) != 1 {
		t.Fatalf("got %d DIDs and diagnostics %#v", len(database.DIDs), database.Diagnostics)
	}

	did := database.DIDs[0]
	if did.Name != "Control_Digital_IO" || did.Identifier != 0x0300 || did.Read == nil || did.Write != nil || did.Read.Length != 2 {
		t.Fatalf("unexpected DID: %#v", did)
	}
	fields := did.Read.Fields
	if len(fields) != 2 || fields[0].BitOffset != 0 || fields[1].BitOffset != 8 {
		t.Fatalf("unexpected fields: %#v", fields)
	}
	if choices := fields[0].Choices; len(choices) != 3 || choices[1].Label != "DIO_HIGH" {
		t.Fatalf("unexpected value labels: %#v", choices)
	}
}

// TestParseRecordLayouts covers the record constructs that determine the
// decoded payload shape. The class also declares an unused write service, so
// successful resolution proves that only services enabled by an instance are
// used to select its data proxy.
func TestParseRecordLayouts(t *testing.T) {
	database, err := cdd.ParseFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}

	thermal, ok := database.DIDByIdentifier(0xf190)
	if !ok || thermal.Read == nil || thermal.Write != nil || thermal.Read.Length != 5 {
		t.Fatalf("unexpected thermal record: %#v", thermal)
	}
	thermalFields := thermal.Read.Fields
	if len(thermalFields) != 3 || thermalFields[0].BitOffset != 0 || thermalFields[1].BitOffset != 16 || thermalFields[2].BitOffset != 32 {
		t.Fatalf("unexpected thermal field offsets: %#v", thermalFields)
	}
	coolant := thermalFields[1]
	if coolant.ByteOrder != cdd.ByteOrderLittle || coolant.Conversion == nil || coolant.Conversion.Scale != 0.5 || coolant.Conversion.Offset != -40 {
		t.Fatalf("unexpected coolant field: %#v", coolant)
	}

	nameplate, ok := database.DIDByIdentifier(0xf191)
	if !ok || nameplate.Read == nil || nameplate.Read.Length != 28 {
		t.Fatalf("unexpected nameplate record: %#v", nameplate)
	}
	nameplateFields := nameplate.Read.Fields
	if len(nameplateFields) != 4 || nameplateFields[0].Count != 12 || nameplateFields[1].BitOffset != 96 || nameplateFields[1].Count != 4 || nameplateFields[2].Encoding != cdd.EncodingFloat {
		t.Fatalf("unexpected nameplate fields: %#v", nameplateFields)
	}

	buffer, ok := database.DIDByIdentifier(0xf192)
	if !ok || buffer.Read == nil || buffer.Read.Length != 3 || buffer.Read.MaxLength != 66 {
		t.Fatalf("unexpected variable record: %#v", buffer)
	}
	bufferFields := buffer.Read.Fields
	if len(bufferFields) != 2 {
		t.Fatalf("unexpected variable fields: %#v", bufferFields)
	}
	variable := bufferFields[1].Variable
	if variable == nil || variable.MinCount != 1 || variable.MaxCount != 64 {
		t.Fatalf("unexpected variable field: %#v", bufferFields[1])
	}

}

// TestParseDropsInvalidDIDs checks that unsupported records cost those records,
// not the usable catalog around them.
func TestParseDropsInvalidDIDs(t *testing.T) {
	database, err := cdd.ParseFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	if len(database.DIDs) != 6 {
		t.Fatalf("got %d DIDs, want 6", len(database.DIDs))
	}
	if _, ok := database.DIDByName("ProgrammingSession"); ok {
		t.Fatal("a session control instance was admitted as a data identifier")
	}
	if _, ok := database.DIDByName("NotInspected"); ok {
		t.Fatal("a DID from a second variant was admitted")
	}

	dropped := make(map[string]string, len(database.Diagnostics))
	for _, diagnostic := range database.Diagnostics {
		dropped[diagnostic.Name] = diagnostic.Message
	}
	checks := map[string]string{
		"ThermalStatusMirror": "identifier 0xf190 is already used",
		"InteriorBuffer":      `variable-length field "Buffer" is followed by DATAOBJ`,
		"CyclicRecord":        "forms a reference cycle",
	}
	if len(dropped) != len(checks) {
		t.Fatalf("unexpected diagnostics: %#v", database.Diagnostics)
	}
	for name, text := range checks {
		if !strings.Contains(dropped[name], text) {
			t.Errorf("DID %q: got %q, want %q", name, dropped[name], text)
		}
	}
}
