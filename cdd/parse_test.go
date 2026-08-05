package cdd_test

import (
	"os"
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
	if len(did.Read.Fields) != 2 || did.Read.Fields[0].BitOffset != 0 || did.Read.Fields[1].BitOffset != 8 {
		t.Fatalf("unexpected fields: %#v", did.Read.Fields)
	}
	if choices := did.Read.Fields[0].Choices; len(choices) != 3 || choices[1].Label != "DIO_HIGH" {
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
	if !ok || thermal.Read == nil || thermal.Write != nil || thermal.Read.Length != 5 || len(thermal.Read.Fields) != 3 {
		t.Fatalf("unexpected thermal record: %#v", thermal)
	}
	if thermal.Read.Fields[0].BitOffset != 0 || thermal.Read.Fields[1].BitOffset != 16 || thermal.Read.Fields[2].BitOffset != 32 {
		t.Fatalf("unexpected thermal field offsets: %#v", thermal.Read.Fields)
	}
	coolant := thermal.Read.Fields[1]
	if coolant.ByteOrder != cdd.ByteOrderLittle || coolant.Conversion == nil || coolant.Conversion.Scale != 0.5 || coolant.Conversion.Offset != -40 {
		t.Fatalf("unexpected coolant field: %#v", coolant)
	}

	nameplate, ok := database.DIDByIdentifier(0xf191)
	if !ok || nameplate.Read == nil || nameplate.Read.Length != 28 || len(nameplate.Read.Fields) != 4 {
		t.Fatalf("unexpected nameplate record: %#v", nameplate)
	}
	if nameplate.Read.Fields[0].Count != 12 || nameplate.Read.Fields[1].BitOffset != 96 || nameplate.Read.Fields[1].Count != 4 || nameplate.Read.Fields[2].Encoding != cdd.EncodingFloat {
		t.Fatalf("unexpected nameplate fields: %#v", nameplate.Read.Fields)
	}

	buffer, ok := database.DIDByIdentifier(0xf192)
	if !ok || buffer.Read == nil || buffer.Read.Length != 3 || buffer.Read.MaxLength != 66 || len(buffer.Read.Fields) != 2 {
		t.Fatalf("unexpected variable record: %#v", buffer)
	}
	variable := buffer.Read.Fields[1].Variable
	if variable == nil || variable.MinCount != 1 || variable.MaxCount != 64 {
		t.Fatalf("unexpected variable field: %#v", buffer.Read.Fields[1])
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

// TestParseCorpus runs the parser over real CDD files that cannot be committed.
func TestParseCorpus(t *testing.T) {
	directory := os.Getenv("GOCAN_CDD_CORPUS")
	if directory == "" {
		t.Skip("set GOCAN_CDD_CORPUS to a directory of CDD files")
	}
	paths, err := filepath.Glob(filepath.Join(directory, "*.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no CDD files in %s", directory)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			database, err := cdd.ParseFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, did := range database.DIDs {
				for _, record := range []*cdd.Record{did.Read, did.Write} {
					if record == nil {
						continue
					}
					var end uint32
					for index, field := range record.Fields {
						if field.BitOffset < end {
							t.Errorf("DID %#04x field %q overlaps its predecessor", did.Identifier, field.Name)
						}
						if field.Variable != nil && index != len(record.Fields)-1 {
							t.Errorf("DID %#04x field %q is variable but not last", did.Identifier, field.Name)
						}
						end = field.BitOffset + field.MaxBitSize()
					}
					if end > record.MaxLength*8 {
						t.Errorf("DID %#04x ends outside its declared payload", did.Identifier)
					}
				}
			}
			t.Logf("%d DIDs, %d dropped", len(database.DIDs), len(database.Diagnostics))
		})
	}
}
