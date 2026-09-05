package cdd_test

import (
	"bytes"
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
	source, err := os.ReadFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	// Rejected layouts and duplicate DIDs must not also emit orphan rule errors.
	source = bytes.ReplaceAll(source, []byte(`<SERVICE tmplref="modernRead" req="0"/>`), []byte(`<SERVICE tmplref="modernRead" req="0" mayBeExec="(9)"/>`))
	database, err := cdd.Parse("rejected.cdd", source)
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
		if _, accepted := database.DIDByName(diagnostic.Name); !accepted {
			if _, exists := dropped[diagnostic.Name]; exists {
				t.Fatalf("orphan precondition diagnostic: %#v", diagnostic)
			}
			dropped[diagnostic.Name] = diagnostic.Message
		}
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

func TestPreconditionsPreserveSources(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(data), `<STATEGROUPS>`, `<STATEGROUPS><STATEGROUP spec="none"><STATE><QUAL>Other</QUAL></STATE></STATEGROUP>`, 1)
	source = strings.Replace(source, `<STATEGROUP spec="security">`, `<STATEGROUP id="security" oid="group-oid" temploid="group-template" spec="security">`, 1)
	source = strings.Replace(source, `<STATE><QUAL>Locked</QUAL></STATE>`, `<STATE id="locked" oid="state-oid" temploid="state-template"><QUAL>Same</QUAL></STATE>`, 1)
	source = strings.Replace(source, `<STATE><QUAL>Unlocked</QUAL></STATE>`, `<STATE id="unlocked"><QUAL>Same</QUAL></STATE>`, 1)
	service := `<SERVICE tmplref="modernRead" req="0" mayBeExec="(1,3,4)"/>`
	source = strings.Replace(source, service, `<SERVICE tmplref="unsupported"/><SERVICE id="a" oid="service-oid" temploid="service-template" tmplref="modernRead" mayBeExec="(1,3,4)"><QUAL>Read</QUAL></SERVICE><SERVICE id="b" tmplref="modernRead" mayBeExec="(1,3,4)"/><SERVICE tmplref="modernRead"/>`, 1)
	database, err := cdd.Parse("sources.cdd", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(database.States) != 6 || database.States[0].Source.Qualifier != "Other" || database.States[0].GroupSpec != "none" || len(database.Sessions) != 3 || database.Sessions[0].Index != 2 || len(database.SecurityLevels) != 2 {
		t.Fatalf("state order was lost: %#v, %#v", database.Sessions, database.SecurityLevels)
	}
	state := database.SecurityLevels[0]
	if state.Source != (cdd.SourceIdentity{ID: "locked", OID: "state-oid", TemplateOID: "state-template", Qualifier: "Same"}) || state.Index != 5 || state.GroupIndex != 3 || state.GroupSpec != "security" || state.Group != (cdd.SourceIdentity{ID: "security", OID: "group-oid", TemplateOID: "group-template", Qualifier: "SecurityAccess"}) {
		t.Fatalf("state identity = %#v", state)
	}
	if database.SecurityLevels[1].Source.ID != "unlocked" || database.SecurityLevels[1].Source.Qualifier != state.Source.Qualifier {
		t.Fatal("equal qualifiers lost distinct state identities")
	}
	did, _ := database.DIDByName("ThermalStatus")
	conditions := did.Read.Preconditions
	if len(conditions) != 3 {
		t.Fatalf("lost service alternatives: %#v", conditions)
	}
	if conditions[0].Service != (cdd.SourceIdentity{ID: "a", OID: "service-oid", TemplateOID: "service-template", Qualifier: "Read"}) || conditions[1].Service.ID != "b" {
		t.Fatalf("service identities = %#v", conditions)
	}
	for i, condition := range conditions {
		if condition.ServiceIndex != i+2 || condition.TemplateRef != "modernRead" {
			t.Fatalf("source order = %#v", condition)
		}
		if i < 2 {
			if condition.Err == nil || condition.MayBeExec == nil || *condition.MayBeExec != "(1,3,4)" {
				t.Fatalf("explicit rule was interpreted without evidence: %#v", condition)
			}
		} else if condition.Err != nil || condition.MayBeExec != nil {
			t.Fatalf("absent rule did not remain unrestricted: %#v", condition)
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
		{"explicit states", `mayBeExec="(3,5)"`, ""},
		{"unknown state", `mayBeExec="(9)"`, ""},
		{"empty selection", `mayBeExec="()"`, ""},
		{"empty attribute", `mayBeExec=""`, ""},
		{"malformed list", `mayBeExec="4"`, ""},
		{"template only", "", `mayBeExec="(3,5)"`},
		{"instance and template", `mayBeExec="(1,4)"`, `mayBeExec="(3,5)"`},
		{"excluded instance group", `notExecInStateGroups="(1)"`, ""},
		{"empty instance exclusion", `notExecInStateGroups=""`, ""},
		{"excluded template group", `mayBeExec="(4)"`, `notExecInStateGroups="(1)"`},
		{"empty template exclusion", "", `notExecInStateGroups=""`},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(string(data), service, `<SERVICE id="affected" tmplref="modernRead" `+test.instance+`/>`, 1)
			source = strings.Replace(source, `<DCLSRVTMPL id="modernRead"`, `<DCLSRVTMPL id="independentRead" tmplref="readByIdentifier" dtref="identifier16" conv="req"/><DCLSRVTMPL `+test.template+` id="modernRead"`, 1)
			source = strings.Replace(source, `<QUAL>ReadWriteCounter</QUAL>`, `<QUAL>ReadWriteCounter</QUAL><SERVICE tmplref="independentRead"/>`, 1)
			source = strings.Replace(source, `<SERVICE tmplref="modernWrite" req="0" mayBeExec="(3,5)"/>`, `<SERVICE tmplref="modernWrite" req="0"/>`, 1)
			database, err := cdd.Parse("unresolved.cdd", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			did, ok := database.DIDByName("ReadWriteCounter")
			if !ok || did.Read == nil || did.Write == nil {
				t.Fatal("DID layout was lost")
			}
			if len(did.Read.Preconditions) != 2 || did.Read.Preconditions[0].Err != nil || did.Read.Preconditions[1].Err == nil {
				t.Fatalf("rule isolation failed: %#v", did.Read.Preconditions)
			}
			condition := did.Read.Preconditions[1]
			if condition.Service.ID != "affected" || condition.ServiceIndex != 2 {
				t.Fatalf("unknown requirement identity = %#v", condition)
			}
			for _, rule := range []struct {
				attrs, name string
				value       *string
			}{
				{test.instance, "mayBeExec", condition.MayBeExec},
				{test.instance, "notExecInStateGroups", condition.NotExecInStateGroups},
				{test.template, "mayBeExec", condition.TemplateMayBeExec},
				{test.template, "notExecInStateGroups", condition.TemplateNotExecInStateGroups},
			} {
				if rule.attrs == "" {
					if rule.value != nil {
						t.Fatalf("absent rule became present: %#v", condition)
					}
				} else if strings.HasPrefix(rule.attrs, rule.name+`="`) {
					if rule.value == nil || rule.name+`="`+*rule.value+`"` != rule.attrs {
						t.Fatalf("raw rule was lost: %#v", condition)
					}
				}
			}
			for _, record := range []*cdd.Record{did.Read, did.Write} {
				payload, err := record.Encode(cdd.Values{"Counter": uint64(7)})
				if err != nil || !bytes.Equal(payload, []byte{7}) {
					t.Fatalf("Encode = %x, %v", payload, err)
				}
				values, err := record.Decode(payload)
				if err != nil || values["Counter"] != uint64(7) {
					t.Fatalf("Decode = %v, %v", values, err)
				}
			}
			if did.Write.Preconditions[0].Err != nil {
				t.Fatal("read precondition failure affected write")
			}
			var reported int
			for _, diagnostic := range database.Diagnostics {
				if diagnostic.Name == did.Name {
					reported++
					if diagnostic.Message != condition.Err.Error() {
						t.Fatalf("diagnostic does not identify the failed alternative: %#v", diagnostic)
					}
				}
			}
			if reported != 1 {
				t.Fatalf("got %d diagnostics for the affected DID, want 1", reported)
			}
		})
	}
}
