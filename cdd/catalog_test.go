package cdd_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tomrford/gocan/cdd"
)

func TestCatalogSelectionAndPresentation(t *testing.T) {
	document, err := cdd.ParseFile(filepath.Join("testdata", "catalog.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	if document.Version != "10.0.108" || len(document.ECUs) != 2 || len(document.ECUs[0].Variants) != 2 {
		t.Fatalf("choices = %#v", document.ECUs)
	}
	if document.DisplayName("en-GB") != "Diagnostic example" || document.DisplayName("fr") != "Diagnosebeispiel" || document.Descriptions.Select("en-GB") != "Use named services.\n\nKeep all alternatives." {
		t.Fatalf("document presentation = %#v", document.Metadata)
	}
	if document.ECUs[0].Variants[0].Base != "1" || document.ECUs[1].Source.ID != "" || document.ECUs[1].Index != 2 || document.ECUs[1].Variants[0].Index != 1 {
		t.Fatal("source choice metadata changed")
	}
	for _, selection := range []cdd.Selection{{}, {ECU: 1}, {ECU: -1}, {ECU: 3}, {ECU: 1, Variant: 3}, {ECU: 2, Variant: -1}} {
		if _, err := document.Select(selection); err == nil {
			t.Fatalf("invalid/ambiguous selection %#v succeeded", selection)
		}
	}
	first, err := document.Select(cdd.Selection{ECU: 1, Variant: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := document.Select(cdd.Selection{ECU: 1, Variant: 2})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := document.Select(cdd.Selection{ECU: 2})
	if err != nil || empty.Selection != (cdd.Selection{ECU: 2, Variant: 1}) || len(empty.Entries) != 0 {
		t.Fatalf("unambiguous variant selection = %#v, %v", empty, err)
	}
	if _, ok := first.DIDByName("OtherMeasurement"); ok {
		t.Fatal("second variant leaked into first")
	}
	if _, ok := second.DIDByName("Measurement"); ok {
		t.Fatal("base variant was silently inherited")
	}
	if len(second.Diagnostics) != 0 || len(second.DIDs) != 1 || second.DIDs[0].Identifier != 0xf191 {
		t.Fatalf("second catalog = %#v", second)
	}
	did, ok := first.DIDByIdentifier(0xf190)
	if !ok || did.Instance != first.Entries[2].Class.Instances[0] || did.Read[0] != did.Instance.Services[0] {
		t.Fatal("DID is not a view into the catalog")
	}
	record := did.Read[0].PositiveResponse.Record
	if record.DisplayName("en-GB") != "Sample record" {
		t.Fatal("record presentation lost")
	}
	field := record.Fields[0]
	if field.Name != "Value" || field.Source.OID != "value-object" || field.DisplayName("de-DE") != "Wert" || field.Descriptions.Select("en-GB") != "Measured value." || field.Datatype.DisplayName("en-GB") != "Byte" || len(field.Groups) != 1 || field.Groups[0].Source.ID != "sample" {
		t.Fatalf("field presentation/grouping = %#v", field)
	}
	if first.Sessions[0].DisplayName("en-GB") != "Default session" || first.Sessions[0].Group.DisplayName("en-GB") != "Sessions" {
		t.Fatal("state presentation lost")
	}
	if did.Read[0].DisplayName("en-GB") != "Read" || did.Read[0].Template.DisplayName("en-GB") != "Read template" {
		t.Fatal("template text merged into source metadata")
	}
	payload, err := record.Encode(cdd.Values{"Value": uint8(42)})
	if err != nil || !bytes.Equal(payload, []byte{42}) {
		t.Fatalf("Encode = %x, %v", payload, err)
	}
	values, err := record.Decode(payload)
	if err != nil || values["Value"] != uint64(42) {
		t.Fatalf("Decode = %#v, %v", values, err)
	}

	// Select reuses immutable XML, not a mutable resolver state or catalog.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			catalog, err := document.Select(cdd.Selection{ECU: 1, Variant: 1})
			if err != nil {
				t.Error(err)
				return
			}
			selected, _ := catalog.DIDByIdentifier(0xf190)
			if selected == did || selected.Read[0] == did.Read[0] {
				t.Error("catalog objects were shared between selections")
			}
			decoded, err := selected.Read[0].PositiveResponse.Record.Decode([]byte{7})
			if err != nil || decoded["Value"] != uint64(7) {
				t.Errorf("independent Decode = %v, %v", decoded, err)
			}
		})
	}
	wg.Wait()
}

func TestServiceCatalogCoverage(t *testing.T) {
	database, err := parseCatalogFile(filepath.Join("testdata", "catalog.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	if len(database.Entries) != 3 || database.Entries[0].Class.Source.Qualifier != "Controls" || database.Entries[2].Class.Source.Qualifier != "Measurements" {
		t.Fatal("class grouping/order changed")
	}
	if direct := database.Entries[1].Instance; direct == nil || direct.Source.Qualifier != "StandaloneReset" || len(direct.Services) != 1 || *direct.Services[0].ServiceID != 0x11 {
		t.Fatal("standalone instance or root order lost")
	}
	services := database.Entries[0].Class.Instances[0].Services

	if len(services) != 8 {
		t.Fatalf("service count = %d", len(services))
	}
	for index, sid := range []uint8{0x10, 0x11, 0x27, 0x31, 0x19, 0x14, 0x2f} {
		service := services[index]
		if service.Index != index+1 || service.Err != nil || service.ServiceID == nil || *service.ServiceID != sid || service.Protocol == nil {
			t.Fatalf("service %d = %#v", index, service)
		}
	}
	if services[0].Protocol.Source.Qualifier != "SessionControl" || services[0].Request.Parameters[1].NumericValue == nil || *services[0].Request.Parameters[1].NumericValue != 3 || services[0].Transitions == nil || *services[0].Transitions != "(1,1)" {
		t.Fatal("session binding or raw transition lost")
	}
	if *services[2].Request.Parameters[1].NumericValue != 5 || *services[3].Request.Parameters[2].NumericValue != 0x1234 || *services[6].Request.Parameters[1].NumericValue != 0x300 {
		t.Fatal("service identifiers lost")
	}
	if services[0].Requirements.Err != nil || len(services[0].Requirements.Sessions) != 1 || len(services[0].Requirements.SecurityLevels) != 1 {
		t.Fatal("non-DID requirements not resolved")
	}
	if services[2].PositiveResponse != nil || services[2].Err != nil {
		t.Fatal("absent response misreported")
	}
	if services[3].Request.Record == nil || services[3].Request.Record.CodecError() != nil {
		t.Fatal("supported request data missing")
	}
	if services[3].PositiveResponse.Err != nil || services[3].PositiveResponse.Record == nil || services[3].PositiveResponse.Record.CodecError() == nil {
		t.Fatal("bit-field metadata/codec coverage lost")
	}
	if services[4].PositiveResponse == nil || services[4].PositiveResponse.Err == nil || services[4].PositiveResponse.Record != nil || !strings.Contains(services[4].PositiveResponse.Err.Error(), "EOSITERCOMP") {
		t.Fatal("unsupported fault record became absent or supported")
	}
	if services[7].TemplateRef != "missing" || services[7].Err == nil || services[7].Requirements.Err == nil || services[7].ServiceID != nil {
		t.Fatal("broken reference disappeared or looked usable")
	}
	for _, expected := range []struct {
		suffix string
		kind   cdd.DiagnosticKind
	}{
		{"SERVICE[4]/POS", cdd.DiagnosticCodec},
		{"SERVICE[5]/POS", cdd.DiagnosticMessage},
		{"SERVICE[6]/REQ", cdd.DiagnosticMessage},
		{"SERVICE[7]/POS", cdd.DiagnosticMessage},
		{"SERVICE[8]", cdd.DiagnosticReference},
	} {
		found := false
		for _, diagnostic := range database.Diagnostics {
			if strings.HasSuffix(diagnostic.Path, expected.suffix) && diagnostic.Kind == expected.kind {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s diagnostic at %s", expected.kind, expected.suffix)
		}
	}
}

func TestInvalidRecordsAndAmbiguousDIDsRemainVisible(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	source = bytes.ReplaceAll(source, []byte(`<SERVICE tmplref="modernRead" req="0"/>`), []byte(`<SERVICE tmplref="modernRead" req="0" mayBeExec="(9)"/>`))
	database, err := parseCatalog("records.cdd", source)
	if err != nil {
		t.Fatal(err)
	}
	if len(database.DIDs) != 9 {
		t.Fatalf("got %d DIDs, want all 9 including unsupported and duplicate entries", len(database.DIDs))
	}
	if _, ok := database.DIDByIdentifier(0xf190); ok {
		t.Fatal("ambiguous identifier selected a DID")
	}
	for _, name := range []string{"ThermalStatus", "ThermalStatusMirror"} {
		did, ok := database.DIDByName(name)
		if !ok || did.Read[0].PositiveResponse.Record.CodecError() != nil {
			t.Fatalf("duplicate lost its usable record: %s", name)
		}
	}
	for _, name := range []string{"InteriorBuffer", "CyclicRecord"} {
		did, ok := database.DIDByName(name)
		if !ok || did.Read[0].PositiveResponse.Err == nil || did.Read[0].PositiveResponse.Record != nil || did.Read[0].Requirements.Err == nil {
			t.Fatalf("unsupported record/rule disappeared: %s", name)
		}
	}
	// Repeated qualifiers make name lookup ambiguous as well, without dropping
	// either instance. Their different identifiers still select each one.
	source = bytes.Replace(source, []byte("<QUAL>UploadBuffer</QUAL>"), []byte("<QUAL>Nameplate</QUAL>"), 1)
	database, err = parseCatalog("duplicate-qualifier.cdd", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := database.DIDByName("Nameplate"); ok {
		t.Fatal("ambiguous qualifier selected a DID")
	}
	for _, id := range []uint16{0xf191, 0xf192} {
		if _, ok := database.DIDByIdentifier(id); !ok {
			t.Fatalf("identifier %#x disappeared", id)
		}
	}
}

func TestServiceReferenceFailuresRemainLocal(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "catalog.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, old, replacement string
		kind                   cdd.DiagnosticKind
	}{
		{"missing protocol", `id="startSession" tmplref="session"`, `id="startSession" tmplref="missing"`, cdd.DiagnosticReference},
		{"wrong reference kind", `id="startSession" tmplref="session"`, `id="startSession" tmplref="u8"`, cdd.DiagnosticReference},
		{"duplicate protocol ID", `<PROTOCOLSERVICES>`, `<PROTOCOLSERVICES><PROTOCOLSERVICE id="session"/>`, cdd.DiagnosticReference},
		{"wrong service owner", `<DCLSRVTMPL id="startSession" tmplref="session"/>`, ``, cdd.DiagnosticReference},
		{"duplicate parameter ID", `<STATICCOMP id="sessionSub" spec="sub" dtref="u8"/>`, `<STATICCOMP id="sessionSub" spec="sub" dtref="u8"/><STATICCOMP id="sessionSub" spec="sub" dtref="u8"/>`, cdd.DiagnosticMessage},
		{"missing static value", `<STATICVALUE shstaticref="sessionValue" v="3"/>`, ``, cdd.DiagnosticMessage},
		{"out of range static", `<STATICVALUE shstaticref="sessionValue" v="3"/>`, `<STATICVALUE shstaticref="sessionValue" v="256"/>`, cdd.DiagnosticMessage},
		{"duplicate static", `<SHSTATIC id="sessionValue" spec="sub">`, `<SHSTATIC id="sessionValue"/><SHSTATIC id="sessionValue" spec="sub">`, cdd.DiagnosticMessage},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(string(data), test.old, test.replacement, 1)
			if source == string(data) {
				t.Fatal("fixture mutation did not apply")
			}
			if test.name == "wrong service owner" {
				source = strings.Replace(source, `</DCLTMPLS>`, `<DCLTMPL id="foreign"><DCLSRVTMPL id="startSession" tmplref="session"/></DCLTMPL></DCLTMPLS>`, 1)
			}
			database, err := parseCatalog("references.cdd", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			services := database.Entries[0].Class.Instances[0].Services
			if len(services) != 8 {
				t.Fatal("broken service was dropped")
			}
			if test.kind == cdd.DiagnosticReference && services[0].Err == nil {
				t.Fatal("unresolved binding was treated as known")
			}
			if test.name == "wrong service owner" {
				if services[0].Requirements.Err == nil {
					t.Fatal("unproven template became known requirements")
				}
			} else if services[0].Requirements.Err != nil || len(services[0].Requirements.Sessions) != 1 {
				t.Fatal("message reference failure affected literal requirements")
			}
			if test.kind == cdd.DiagnosticMessage && (services[0].Request == nil || services[0].Request.Err == nil) {
				t.Fatal("invalid parameter was treated as usable")
			}
			if services[1].Err != nil || *services[1].ServiceID != 0x11 {
				t.Fatal("sibling service was affected")
			}
			did, ok := database.DIDByIdentifier(0xf190)
			if !ok {
				t.Fatal("independent DID was lost")
			}
			values, err := did.Read[0].PositiveResponse.Record.Decode([]byte{9})
			if err != nil || values["Value"] != uint64(9) {
				t.Fatalf("independent codec = %v, %v", values, err)
			}
			found := false
			for _, d := range database.Diagnostics {
				if d.Source.ID == "sessionService" && d.Kind == test.kind {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %s diagnostic", test.kind)
			}
		})
	}
}

func TestServiceAlternativesKeepDistinctLayouts(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(data), `<DCLSRVTMPL id="modernRead"`, `<DCLSRVTMPL id="otherRead" tmplref="otherProtocol"/><DCLSRVTMPL id="modernRead"`, 1)
	source = strings.Replace(source, `</PROTOCOLSERVICES>`, `<PROTOCOLSERVICE id="otherProtocol"><REQ><CONSTCOMP spec="sid" bl="8" v="34"/><STATICCOMP id="otherID" spec="id" dtref="identifier16"/></REQ><POS><SIMPLEPROXYCOMP id="otherData" dest="data"/></POS></PROTOCOLSERVICE></PROTOCOLSERVICES>`, 1)
	source = strings.Replace(source, `<STATICCOMPREF idref="readIdentifier"/>`, `<STATICCOMPREF idref="readIdentifier"/><STATICCOMPREF idref="otherID"/>`, 1)
	source = strings.Replace(source, `<SHPROXY id="modernData"`, `<SHPROXY id="otherProxy" dest="data"><PROXYCOMPREF idref="otherData"/></SHPROXY><SHPROXY id="modernData"`, 1)
	source = strings.Replace(source, `<QUAL>ReadWriteCounter</QUAL>`, `<QUAL>ReadWriteCounter</QUAL><SERVICE tmplref="otherRead"/><SIMPLECOMPCONT shproxyref="otherProxy"><DATAOBJ dtref="block4"><QUAL>Counter</QUAL></DATAOBJ></SIMPLECOMPCONT>`, 1)
	database, err := parseCatalog("alternatives.cdd", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	did, ok := database.DIDByName("ReadWriteCounter")
	if !ok || len(did.Read) != 2 {
		t.Fatal("service alternatives lost")
	}
	wide, narrow := did.Read[0].PositiveResponse.Record, did.Read[1].PositiveResponse.Record
	if wide == nil || narrow == nil || wide.Length != 4 || narrow.Length != 1 {
		t.Fatal("layouts were merged across service alternatives")
	}
	values, err := wide.Decode([]byte{1, 2, 3, 4})
	if err != nil || len(values["Counter"].([]uint64)) != 4 {
		t.Fatalf("wide Decode = %v, %v", values, err)
	}
	values, err = narrow.Decode([]byte{9})
	if err != nil || values["Counter"] != uint64(9) {
		t.Fatalf("narrow Decode = %v, %v", values, err)
	}
}

func TestSIDFailuresAreMessageDiagnostics(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "catalog.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, old, replacement string }{
		{"missing SID", `<CONSTCOMP spec="sid" bl="8" v="16"/>`, ``},
		{"wide SID", `<CONSTCOMP spec="sid" bl="8" v="16"/>`, `<CONSTCOMP spec="sid" bl="16" v="16"/>`},
		{"duplicate SID", `<CONSTCOMP spec="sid" bl="8" v="16"/>`, `<CONSTCOMP spec="sid" bl="8" v="16"/><CONSTCOMP spec="sid" bl="8" v="16"/>`},
		{"missing REQ", `<REQ><CONSTCOMP spec="sid" bl="8" v="16"/><STATICCOMP id="sessionSub" spec="sub" dtref="u8"/></REQ>`, ``},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(string(data), test.old, test.replacement, 1)
			database, err := parseCatalog("sid.cdd", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			service := database.Entries[0].Class.Instances[0].Services[0]
			if service.Protocol == nil || service.ServiceID != nil || service.Err == nil || service.Requirements.Err != nil {
				t.Fatal("SID failure changed binding or requirement coverage")
			}
			var messages int
			for _, d := range database.Diagnostics {
				if d.Source.ID != "sessionService" {
					continue
				}
				if d.Kind != cdd.DiagnosticMessage || !strings.HasSuffix(d.Path, "/SERVICE[1]/REQ") {
					t.Fatalf("SID diagnostic misclassified: %#v", d)
				}
				messages++
			}
			if messages != 1 {
				t.Fatalf("got %d SID diagnostics, want one", messages)
			}
		})
	}
}
