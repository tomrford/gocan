package dbc

import (
	"bytes"
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
			reparsed, err := Parse(fixture+"-written.dbc", strings.NewReader(written.String()))
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
	wantCopy.source, gotCopy.source = nil, nil
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

// Edit hands its callback a database that shares nothing mutable with the
// original: rewriting every reachable value in place leaves the original as
// parsed, and an edit that changes nothing derives an equivalent database.
func TestEditIsolatesOriginal(t *testing.T) {
	for _, fixture := range []string{"core", "codec", "multiplex_j1939", "roundtrip", "dropped"} {
		t.Run(fixture, func(t *testing.T) {
			path := "testdata/" + fixture + ".dbc"
			original, pristine := parseFixture(t, path), parseFixture(t, path)

			unchanged, err := original.Edit(fixture+"-unchanged.dbc", func(*Database) {})
			if err != nil {
				t.Fatal(err)
			}
			assertEquivalent(t, original, unchanged)

			// The scribbled copy need not be a valid database; only the
			// original matters here.
			_, _ = original.Edit(fixture+"-scribbled.dbc", func(db *Database) { scribble(reflect.ValueOf(db).Elem()) })
			assertEquivalent(t, pristine, original)
			if !reflect.DeepEqual(pristine.Diagnostics, original.Diagnostics) || !bytes.Equal(pristine.Source(), original.Source()) {
				t.Fatal("Edit changed the original diagnostics or source")
			}
		})
	}
}

// scribble changes every exported value reachable from v in place, through
// pointers, slices and maps, so any storage shared with another value shows.
func scribble(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			scribble(v.Elem())
		}
	case reflect.Struct:
		for index := range v.NumField() {
			if v.Type().Field(index).IsExported() {
				scribble(v.Field(index))
			}
		}
	case reflect.Slice:
		for index := range v.Len() {
			scribble(v.Index(index))
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			value := reflect.New(v.Type().Elem()).Elem()
			value.Set(v.MapIndex(key))
			scribble(value)
			v.SetMapIndex(key, value)
		}
		if !v.IsNil() && v.Type().Key().Kind() == reflect.String {
			v.SetMapIndex(reflect.ValueOf("scribbled").Convert(v.Type().Key()), reflect.Zero(v.Type().Elem()))
		}
	case reflect.Interface:
		v.SetZero()
	case reflect.String:
		v.SetString(v.String() + "~")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(v.Uint() + 1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 1)
	}
}
