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

func TestServicePreconditionsPreserveSources(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(data), `<STATEGROUPS>`, `<STATEGROUPS><STATEGROUP spec="none"><STATE><QUAL>Other</QUAL></STATE></STATEGROUP>`, 1)
	source = strings.Replace(source, `<STATEGROUP spec="security">`, `<STATEGROUP id="security" oid="group-oid" temploid="group-template" spec="security">`, 1)
	source = strings.Replace(source, `<STATE><QUAL>Locked</QUAL></STATE>`, `<STATE id="locked" oid="state-oid" temploid="state-template"><QUAL>Same</QUAL></STATE>`, 1)
	source = strings.Replace(source, `<STATE><QUAL>Unlocked</QUAL></STATE>`, `<STATE id="unlocked"><QUAL>Same</QUAL></STATE>`, 1)
	service := `<SERVICE tmplref="modernRead" req="0" mayBeExec="(1,3,4)"/>`
	source = strings.Replace(source, service, `<SERVICE tmplref="unsupported"/><SERVICE id="a" oid="service-oid" temploid="service-template" tmplref="modernRead" mayBeExec="(2,4,5,6)"><QUAL>Read</QUAL></SERVICE><SERVICE id="b" tmplref="modernRead" mayBeExec="(6,5,4,2,4)"/><SERVICE tmplref="modernRead"/>`, 1)
	database, err := parseCatalog("sources.cdd", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(database.States) != 6 || database.States[0].Source.Qualifier != "Other" || len(database.Sessions) != 3 || database.Sessions[0].Index != 2 || len(database.SecurityLevels) != 2 {
		t.Fatal("state source order was lost")
	}
	state := database.SecurityLevels[0]
	if state.Source != (cdd.SourceIdentity{ID: "locked", OID: "state-oid", TemplateOID: "state-template", Qualifier: "Same"}) || state.Index != 5 || state.GroupIndex != 3 || state.GroupSpec != "security" || state.Group.Source != (cdd.SourceIdentity{ID: "security", OID: "group-oid", TemplateOID: "group-template", Qualifier: "SecurityAccess"}) {
		t.Fatalf("state identity = %#v", state)
	}
	if database.SecurityLevels[1].Source.ID != "unlocked" || database.SecurityLevels[1].Source.Qualifier != state.Source.Qualifier {
		t.Fatal("equal qualifiers lost distinct identities")
	}
	did, _ := database.DIDByName("ThermalStatus")
	if len(did.Instance.Services) != 4 || len(did.Read) != 3 {
		t.Fatal("service alternatives were lost")
	}
	unknown := did.Instance.Services[0]
	if unknown.Err == nil || unknown.Requirements.Err == nil || unknown.Index != 1 {
		t.Fatal("unknown binding appeared unrestricted or disappeared")
	}
	if did.Read[0].Source != (cdd.SourceIdentity{ID: "a", OID: "service-oid", TemplateOID: "service-template", Qualifier: "Read"}) || did.Read[1].Source.ID != "b" {
		t.Fatal("service source identities were lost")
	}
	for i, service := range did.Read {
		if service != did.Instance.Services[i+1] || service.Index != i+2 || service.TemplateRef != "modernRead" {
			t.Fatal("DID view is not the original service")
		}
		condition := service.Requirements
		if i < 2 {
			if condition.Err != nil || condition.MayBeExec == nil || !reflect.DeepEqual(condition.Sessions, []cdd.State{database.States[1], database.States[3]}) || !reflect.DeepEqual(condition.SecurityLevels, database.SecurityLevels) {
				t.Fatalf("literal rule = %#v", condition)
			}
		} else if condition.Err != nil || condition.MayBeExec != nil || condition.Sessions != nil || condition.SecurityLevels != nil {
			t.Fatal("absent rule gained requirements")
		}
	}
}

func TestServicePreconditionsKeepLiteralStates(t *testing.T) {
	database, err := parseCatalogFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	counter, _ := database.DIDByName("ReadWriteCounter")
	read, write := counter.Read[0].Requirements, counter.Write[0].Requirements
	if read.Err != nil || read.Sessions != nil || !reflect.DeepEqual(read.SecurityLevels, []cdd.State{database.States[3]}) || read.SecurityLevels[0].Source.Qualifier != "Locked" {
		t.Fatalf("literal locked state was changed: %#v", read)
	}
	if write.Err != nil || !reflect.DeepEqual(write.Sessions, []cdd.State{database.States[2]}) || !reflect.DeepEqual(write.SecurityLevels, []cdd.State{database.States[4]}) {
		t.Fatalf("write requirements = %#v", write)
	}
}

func TestUnknownRulesKeepSiblingServicesAndCodecs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	service := `<SERVICE tmplref="modernRead" req="0" mayBeExec="(4)"/>`
	for _, test := range []struct{ name, instance, template string }{
		{"unknown state", `mayBeExec="(9)"`, ""},
		{"zero index", `mayBeExec="(0)"`, ""},
		{"unsupported group after valid state", `mayBeExec="(4,6)"`, ""},
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
			source = strings.Replace(source, `<QUAL>ReadWriteCounter</QUAL>`, `<QUAL>ReadWriteCounter</QUAL><SERVICE tmplref="independentRead" mayBeExec="(4)"/>`, 1)
			source = strings.Replace(source, `</STATEGROUPS>`, `<STATEGROUP spec="none"><STATE><QUAL>Other</QUAL></STATE></STATEGROUP></STATEGROUPS>`, 1)
			database, err := parseCatalog("unresolved.cdd", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			did, ok := database.DIDByName("ReadWriteCounter")
			if !ok || len(did.Read) != 2 || len(did.Write) != 1 {
				t.Fatal("services were lost")
			}
			condition := did.Read[1].Requirements
			if did.Read[0].Requirements.Err != nil || condition.Err == nil || did.Write[0].Requirements.Err != nil {
				t.Fatal("rule isolation failed")
			}
			if condition.Sessions != nil || condition.SecurityLevels != nil || !reflect.DeepEqual(did.Read[0].Requirements.SecurityLevels, []cdd.State{database.States[3]}) {
				t.Fatal("failed rule left partial states or affected sibling")
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
				if rule.attrs == "" && rule.value != nil {
					t.Fatal("absent attribute became present")
				}
				if strings.HasPrefix(rule.attrs, rule.name+`="`) && (rule.value == nil || rule.name+`="`+*rule.value+`"` != rule.attrs) {
					t.Fatal("raw rule was lost")
				}
			}
			for _, record := range []*cdd.Record{did.Read[1].PositiveResponse.Record, did.Write[0].Request.Record} {
				payload, err := record.Encode(cdd.Values{"Counter": uint64(7)})
				if err != nil || !bytes.Equal(payload, []byte{7}) {
					t.Fatalf("Encode = %x, %v", payload, err)
				}
				values, err := record.Decode(payload)
				if err != nil || values["Counter"] != uint64(7) {
					t.Fatalf("Decode = %v, %v", values, err)
				}
			}
			var reported int
			for _, diagnostic := range database.Diagnostics {
				if diagnostic.Source.ID == "affected" && diagnostic.Kind == cdd.DiagnosticPrecondition {
					reported++
					if diagnostic.Message != condition.Err.Error() || !strings.HasSuffix(diagnostic.Path, "/SERVICE[2]") {
						t.Fatalf("diagnostic = %#v", diagnostic)
					}
				}
			}
			if reported != 1 {
				t.Fatalf("got %d diagnostics for affected rule", reported)
			}
		})
	}
}
