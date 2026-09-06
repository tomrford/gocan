package cdd_test

import (
	"path/filepath"
	"testing"

	"github.com/tomrford/gocan/cdd"
)

// TestParseVectorDocument fixes the resolution required by real Vector input:
// the identifier width and record layout both arrive through references.
func TestParseVectorDocument(t *testing.T) {
	document, err := cdd.ParseFile(filepath.Join("testdata", "vector-diddataref.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	database, err := document.Select(cdd.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(database.Diagnostics) != 0 || len(database.DIDs) != 1 {
		t.Fatalf("got %d DIDs and diagnostics %#v", len(database.DIDs), database.Diagnostics)
	}

	did := database.DIDs[0]
	if did.Instance.Source.Qualifier != "Control_Digital_IO" || did.Identifier != 0x0300 || did.Read == nil || did.Write != nil || did.Read[0].PositiveResponse.Record.Length != 2 {
		t.Fatalf("unexpected DID: %#v", did)
	}
	fields := did.Read[0].PositiveResponse.Record.Fields
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
	database, err := parseCatalogFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}

	thermal, ok := database.DIDByName("ThermalStatus")
	if !ok || thermal.Read == nil || thermal.Write != nil || thermal.Read[0].PositiveResponse.Record.Length != 5 {
		t.Fatalf("unexpected thermal record: %#v", thermal)
	}
	thermalFields := thermal.Read[0].PositiveResponse.Record.Fields
	if len(thermalFields) != 3 || thermalFields[0].BitOffset != 0 || thermalFields[1].BitOffset != 16 || thermalFields[2].BitOffset != 32 {
		t.Fatalf("unexpected thermal field offsets: %#v", thermalFields)
	}
	coolant := thermalFields[1]
	if coolant.ByteOrder != cdd.ByteOrderLittle || coolant.Conversion == nil || coolant.Conversion.Scale != 0.5 || coolant.Conversion.Offset != -40 {
		t.Fatalf("unexpected coolant field: %#v", coolant)
	}

	nameplate, ok := database.DIDByIdentifier(0xf191)
	if !ok || nameplate.Read == nil || nameplate.Read[0].PositiveResponse.Record.Length != 28 {
		t.Fatalf("unexpected nameplate record: %#v", nameplate)
	}
	nameplateFields := nameplate.Read[0].PositiveResponse.Record.Fields
	if len(nameplateFields) != 4 || nameplateFields[0].Count != 12 || nameplateFields[1].BitOffset != 96 || nameplateFields[1].Count != 4 || nameplateFields[2].Encoding != cdd.EncodingFloat {
		t.Fatalf("unexpected nameplate fields: %#v", nameplateFields)
	}

	buffer, ok := database.DIDByIdentifier(0xf192)
	if !ok || buffer.Read == nil || buffer.Read[0].PositiveResponse.Record.Length != 3 || buffer.Read[0].PositiveResponse.Record.MaxLength != 66 {
		t.Fatalf("unexpected variable record: %#v", buffer)
	}
	bufferFields := buffer.Read[0].PositiveResponse.Record.Fields
	if len(bufferFields) != 2 {
		t.Fatalf("unexpected variable fields: %#v", bufferFields)
	}
	variable := bufferFields[1].Variable
	if variable == nil || variable.MinCount != 1 || variable.MaxCount != 64 {
		t.Fatalf("unexpected variable field: %#v", bufferFields[1])
	}

}

// Helpers select the primary fixture variant explicitly; selection behaviour
// itself is exercised with multiple ECUs and variants in TestCatalogSelection.
func parseCatalogFile(path string) (*cdd.Database, error) {
	document, err := cdd.ParseFile(path)
	if err != nil {
		return nil, err
	}
	return document.Select(cdd.Selection{ECU: 1, Variant: 1})
}

func parseCatalog(name string, source []byte) (*cdd.Database, error) {
	document, err := cdd.Parse(name, source)
	if err != nil {
		return nil, err
	}
	return document.Select(cdd.Selection{ECU: 1, Variant: 1})
}
