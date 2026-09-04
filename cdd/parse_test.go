package cdd_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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

// TestParsePreconditions covers execution preconditions: state indexes resolve
// to session and security names, a group with no listed state and a service
// with no attribute both leave the operation unrestricted, and an index outside
// the declared states is reported without costing the DID.
func TestParsePreconditions(t *testing.T) {
	path := filepath.Join("testdata", "records.cdd")
	database, err := cdd.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(database.Sessions, []string{"Default", "Programming", "Extended"}) || !reflect.DeepEqual(database.SecurityLevels, []string{"Locked", "Unlocked"}) {
		t.Fatalf("unexpected states: %#v %#v", database.Sessions, database.SecurityLevels)
	}
	thermal, _ := database.DIDByName("ThermalStatus")
	if len(thermal.Read.Preconditions) != 1 || thermal.Read.Preconditions[0].Err != nil {
		t.Fatalf("thermal preconditions = %#v", thermal.Read.Preconditions)
	}
	if !reflect.DeepEqual(thermal.Read.Preconditions[0].Sessions, []string{"Default", "Extended"}) || !reflect.DeepEqual(thermal.Read.Preconditions[0].SecurityLevels, []string{"Locked"}) {
		t.Fatalf("unexpected thermal preconditions: %#v %#v", thermal.Read.Preconditions[0].Sessions, thermal.Read.Preconditions[0].SecurityLevels)
	}
	counter, _ := database.DIDByName("ReadWriteCounter")
	if counter.Read.Preconditions[0].Sessions != nil || !reflect.DeepEqual(counter.Read.Preconditions[0].SecurityLevels, []string{"Locked"}) {
		t.Fatalf("unexpected counter read preconditions: %#v %#v", counter.Read.Preconditions[0].Sessions, counter.Read.Preconditions[0].SecurityLevels)
	}
	if !reflect.DeepEqual(counter.Write.Preconditions[0].Sessions, []string{"Extended"}) || !reflect.DeepEqual(counter.Write.Preconditions[0].SecurityLevels, []string{"Unlocked"}) {
		t.Fatalf("unexpected counter write preconditions: %#v %#v", counter.Write.Preconditions[0].Sessions, counter.Write.Preconditions[0].SecurityLevels)
	}
	settings, _ := database.DIDByName("WritableSettings")
	if settings.Write.Preconditions[0].Sessions != nil || settings.Write.Preconditions[0].SecurityLevels != nil {
		t.Fatalf("a service without mayBeExec was restricted: %#v %#v", settings.Write.Preconditions[0].Sessions, settings.Write.Preconditions[0].SecurityLevels)
	}

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutated := bytes.Replace(source, []byte(`mayBeExec="(3,5)"`), []byte(`mayBeExec="(3,9)"`), 1)
	if bytes.Equal(mutated, source) {
		t.Fatal("fixture no longer carries the write precondition to corrupt")
	}
	database, err = cdd.Parse("records.cdd", mutated)
	if err != nil {
		t.Fatal(err)
	}
	counter, ok := database.DIDByName("ReadWriteCounter")
	if !ok || counter.Write == nil || counter.Write.Preconditions[0].Err == nil || counter.Write.Preconditions[0].Sessions != nil || counter.Write.Preconditions[0].SecurityLevels != nil {
		t.Fatalf("an unresolved precondition did not preserve the DID: %#v", counter)
	}
	var reported int
	for _, diagnostic := range database.Diagnostics {
		if diagnostic.Name == "ReadWriteCounter" && strings.Contains(diagnostic.Message, `write service: records.cdd: mayBeExec "(3,9)" names a state outside the 5 declared states`) {
			reported++
		}
	}
	if reported != 1 {
		t.Fatalf("unexpected diagnostics: %#v", database.Diagnostics)
	}
}

func TestPreconditionsPreserveAlternatives(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(data), `<DCLSRVTMPL id="modernRead"`, `<DCLSRVTMPL id="alternateRead" tmplref="readByIdentifier" dtref="identifier16" conv="req"/><DCLSRVTMPL id="modernRead"`, 1)
	first := `<SERVICE tmplref="modernRead" req="0" mayBeExec="(1,3,4)"/>`
	second := `<SERVICE tmplref="alternateRead" req="0" mayBeExec="(2,5)"/>`
	equivalent := `<SERVICE tmplref="modernRead" req="0" mayBeExec="(4,3,1,3)"/>`
	a := cdd.Precondition{Sessions: []string{"Default", "Extended"}, SecurityLevels: []string{"Locked"}}
	b := cdd.Precondition{Sessions: []string{"Programming"}, SecurityLevels: []string{"Unlocked"}}
	for _, test := range []struct {
		services string
		want     []cdd.Precondition
	}{
		{first + second + equivalent, []cdd.Precondition{a, b}},
		{second + first + equivalent, []cdd.Precondition{b, a}},
	} {
		database, err := cdd.Parse("alternatives.cdd", []byte(strings.Replace(source, first, test.services, 1)))
		if err != nil {
			t.Fatal(err)
		}
		did, ok := database.DIDByName("ThermalStatus")
		if !ok || did.Read == nil {
			t.Fatal("DID layout was lost")
		}
		if !reflect.DeepEqual(did.Read.Preconditions, test.want) {
			t.Fatalf("alternatives = %#v, want %#v", did.Read.Preconditions, test.want)
		}
	}
}

func TestUnresolvedPreconditionsKeepTheCodec(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	service := `<SERVICE tmplref="modernRead" req="0" mayBeExec="(4)"/>`
	for _, test := range []struct{ name, instance, template string }{
		{"unknown state", `mayBeExec="(9)"`, ""},
		{"empty selection", `mayBeExec="()"`, ""},
		{"malformed list", `mayBeExec="4"`, ""},
		{"template only", "", `mayBeExec="(3,5)"`},
		{"excluded template group", `mayBeExec="(4)"`, `notExecInStateGroups="(1)"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(string(data), service, `<SERVICE tmplref="modernRead" req="0" `+test.instance+`/>`, 1)
			source = strings.Replace(source, `<DCLSRVTMPL id="modernRead"`, `<DCLSRVTMPL `+test.template+` id="modernRead"`, 1)
			database, err := cdd.Parse("unresolved.cdd", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			did, ok := database.DIDByName("ReadWriteCounter")
			if !ok || did.Read == nil {
				t.Fatal("DID layout was lost")
			}
			if len(did.Read.Preconditions) != 1 || did.Read.Preconditions[0].Err == nil {
				t.Fatal("unresolved rule became unrestricted")
			}
			values, err := did.Read.Decode([]byte{7})
			if err != nil || values["Counter"] != uint64(7) {
				t.Fatalf("Decode = %v, %v", values, err)
			}
			if did.Write.Preconditions[0].Err != nil {
				t.Fatal("read precondition failure affected write")
			}
		})
	}
}
