package dbc

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestWriteRoundTrip(t *testing.T) {
	for _, fixture := range []string{"core", "codec", "multiplex_j1939", "roundtrip", "dropped"} {
		t.Run(fixture, func(t *testing.T) {
			original := parseFixture(t, "testdata/"+fixture+".dbc")

			var written strings.Builder
			if err := original.Write(&written); err != nil {
				t.Fatal(err)
			}
			reparsed, err := Parse(fixture+"-written.dbc", written.String())
			if err != nil {
				t.Fatalf("written database does not parse: %v\n%s", err, written.String())
			}
			assertEquivalent(t, original, reparsed)

			// Records the model drops stay dropped; everything written is
			// understood, so only diagnostics about kept content recur.
			if fixture == "dropped" {
				if len(original.Diagnostics) == 0 || len(reparsed.Diagnostics) != 0 {
					t.Fatalf("diagnostics: original %d, reparsed %#v", len(original.Diagnostics), reparsed.Diagnostics)
				}
			} else if want, got := diagnosticMessages(original), diagnosticMessages(reparsed); !slices.Equal(want, got) {
				t.Fatalf("diagnostics changed:\nwant %q\ngot  %q", want, got)
			}

			var rewritten strings.Builder
			if err := reparsed.Write(&rewritten); err != nil {
				t.Fatal(err)
			}
			if rewritten.String() != written.String() {
				t.Fatalf("rewriting the reparsed database changed the text:\n%s\n---\n%s", written.String(), rewritten.String())
			}
		})
	}
}

// assertEquivalent compares two resolved databases, including their derived
// lookups and codecs, apart from diagnostics and source text.
func assertEquivalent(t *testing.T, want, got *Database) {
	t.Helper()
	wantCopy, gotCopy := *want, *got
	wantCopy.Diagnostics, gotCopy.Diagnostics = nil, nil
	wantCopy.source, gotCopy.source = "", ""
	if reflect.DeepEqual(wantCopy, gotCopy) {
		return
	}
	if len(want.Messages) == len(got.Messages) {
		for index := range want.Messages {
			if !reflect.DeepEqual(want.Messages[index], got.Messages[index]) {
				t.Fatalf("message %d differs:\nwant %#v\ngot  %#v", index, want.Messages[index], got.Messages[index])
			}
		}
	}
	wantCopy.Messages, gotCopy.Messages = nil, nil
	t.Fatalf("databases differ:\nwant %#v\ngot  %#v", wantCopy, gotCopy)
}

func diagnosticMessages(db *Database) []string {
	var messages []string
	for _, diagnostic := range db.Diagnostics {
		messages = append(messages, diagnostic.Keyword+": "+diagnostic.Message)
	}
	slices.Sort(messages)
	return messages
}
