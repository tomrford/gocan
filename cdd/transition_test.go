package cdd_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tomrford/gocan/cdd"
)

func TestTransitionsPreserveIndependentGroupEffects(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(data), `</STATEGROUPS>`, `<STATEGROUP id="communication" spec="none"><QUAL>Communication</QUAL><STATE id="enabled"><QUAL>Same</QUAL></STATE><STATE id="disabled"><QUAL>Same</QUAL></STATE></STATEGROUP></STATEGROUPS>`, 1)
	source = strings.Replace(source, `<SERVICE tmplref="modernRead" req="0" mayBeExec="(4)"/>`, `<SERVICE tmplref="modernRead" mayBeExec="(1,3,4,5)" trans="(3,1,5,4,4,4,6,7,3,1)"/>`, 1)
	// Unresolved template preconditions must not conceal literal transitions.
	source = strings.Replace(source, `<DCLSRVTMPL id="modernRead"`, `<DCLSRVTMPL mayBeExec="(2,3)" id="modernRead"`, 1)
	database, err := parseCatalog("transitions.cdd", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	did, _ := database.DIDByName("ReadWriteCounter")
	service := did.Read[0]
	rule := service.Transitions
	want := []cdd.StateTransition{
		{From: database.States[2], To: database.States[0]},
		{From: database.States[4], To: database.States[3]},
		{From: database.States[3], To: database.States[3]},
		{From: database.States[5], To: database.States[6]},
		{From: database.States[2], To: database.States[0]},
	}
	if rule.Err != nil || !reflect.DeepEqual(rule.Pairs, want) {
		t.Fatalf("transitions = %#v, error %v", rule.Pairs, rule.Err)
	}
	if rule.Pairs[3].From.Group.Source.ID != "communication" || rule.Pairs[3].From.Source.ID != "enabled" || rule.Pairs[3].To.Source.ID != "disabled" {
		t.Fatal("equal state names lost group or endpoint identities")
	}
	if service.Requirements.Err == nil || service.Requirements.Sessions != nil || service.Requirements.SecurityLevels != nil {
		t.Fatal("transitions invented a resolution for template preconditions")
	}
	if absent := did.Write[0].Transitions; absent.Err != nil || absent.Trans != nil || absent.Pairs != nil {
		t.Fatal("missing transitions gained effects")
	}
}

func TestUnknownTransitionsRemainIsolated(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "records.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, instance, template string }{
		{"empty", `trans=""`, ""},
		{"empty list", `trans="()"`, ""},
		{"odd list", `trans="(1,3,4)"`, ""},
		{"missing parentheses", `trans="1,3"`, ""},
		{"missing endpoint", `trans="(1,)"`, ""},
		{"zero endpoint", `trans="(0,1)"`, ""},
		{"out of range after valid pair", `trans="(1,3,4,99)"`, ""},
		{"cross group after valid pair", `trans="(1,3,4,1)"`, ""},
		{"cross group with same semantic", `trans="(4,6)"`, ""},
		{"conflicting destination", `trans="(1,3,1,2)"`, ""},
		{"template only", "", `trans="(1,3)"`},
		{"instance and template", `trans="(1,2)"`, `trans="(1,3)"`},
		{"empty template", `trans="(1,3)"`, `trans=""`},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(string(data), `<SERVICE tmplref="modernRead" req="0" mayBeExec="(4)"/>`, `<SERVICE id="affected" tmplref="modernRead" mayBeExec="(4)" `+test.instance+`/>`, 1)
			source = strings.Replace(source, `<DCLSRVTMPL id="modernRead"`, `<DCLSRVTMPL `+test.template+` id="modernRead"`, 1)
			source = strings.Replace(source, `</STATEGROUPS>`, `<STATEGROUP spec="security"><STATE><QUAL>SeparateLock</QUAL></STATE></STATEGROUP></STATEGROUPS>`, 1)
			database, err := parseCatalog("unknown-transitions.cdd", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			did, _ := database.DIDByName("ReadWriteCounter")
			service := did.Read[0]
			rule := service.Transitions
			if rule.Err == nil || rule.Pairs != nil || service.Requirements.Err != nil || did.Write[0].Transitions.Err != nil {
				t.Fatal("unknown transitions leaked partial pairs or affected other metadata")
			}
			for _, raw := range []struct {
				attribute string
				value     *string
			}{{test.instance, rule.Trans}, {test.template, rule.TemplateTrans}} {
				if raw.attribute == "" {
					if raw.value != nil {
						t.Fatal("absent attribute became present")
					}
				} else if raw.value == nil || `trans="`+*raw.value+`"` != raw.attribute {
					t.Fatal("raw transition rule was lost")
				}
			}
			values, err := service.PositiveResponse.Record.Decode([]byte{7})
			if err != nil || values["Counter"] != uint64(7) {
				t.Fatalf("unrelated decode = %v, %v", values, err)
			}
			var reported int
			for _, diagnostic := range database.Diagnostics {
				if diagnostic.Source.ID == "affected" && diagnostic.Kind == cdd.DiagnosticTransition {
					reported++
					if diagnostic.Message != rule.Err.Error() || !strings.HasSuffix(diagnostic.Path, "/SERVICE[1]") {
						t.Fatalf("diagnostic = %#v", diagnostic)
					}
				}
			}
			if reported != 1 {
				t.Fatalf("got %d transition diagnostics", reported)
			}
		})
	}
}

func TestUnresolvedBindingDoesNotAppearToHaveNoTransitions(t *testing.T) {
	database, err := parseCatalogFile(filepath.Join("testdata", "catalog.cdd"))
	if err != nil {
		t.Fatal(err)
	}
	unknown := database.Entries[0].Class.Instances[0].Services[7]
	if unknown.Err == nil || unknown.Transitions.Err == nil || unknown.Transitions.Pairs != nil {
		t.Fatal("unresolved binding appeared to have known transition behaviour")
	}
}
