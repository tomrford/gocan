package dbc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFile(t *testing.T) {
	const (
		prefix = "VERSION \"1.0\"\n" +
			"BU_: ECU\n" +
			"BO_ 256 Status: 1 ECU\n" +
			" SG_ State : 0|8@1+ (1,0) [0|255] \"Fahrzeuggr"
		suffix = "e\" ECU\nCM_ \"ignored comment\";\nFILTER ignored\n"
	)

	utf8Source := append([]byte("\xef\xbb\xbf"), []byte(prefix+"öß"+suffix)...)
	windows1252Source := append([]byte(prefix), 0xf6, 0xdf)
	windows1252Source = append(windows1252Source, suffix...)

	path := filepath.Join(t.TempDir(), "vehicle.dbc")
	parse := func(source []byte) *Database {
		t.Helper()
		if err := os.WriteFile(path, source, 0o600); err != nil {
			t.Fatal(err)
		}
		db, err := ParseFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}

	for encoding, db := range map[string]*Database{
		"UTF-8 with BOM": parse(utf8Source),
		"Windows-1252":   parse(windows1252Source),
	} {
		t.Run(encoding, func(t *testing.T) {
			if db.Version != "1.0" {
				t.Fatalf("unexpected database version: %q", db.Version)
			}
			if len(db.Nodes) != 1 || db.Nodes[0].Name != "ECU" || len(db.Messages) != 1 {
				t.Fatalf("unexpected resolved model: nodes=%#v messages=%#v", db.Nodes, db.Messages)
			}
			message := db.Messages[0]
			if message.ID != 256 || message.Name != "Status" || len(message.Signals) != 1 ||
				message.Signals[0].Name != "State" || message.Signals[0].Unit != "Fahrzeuggröße" {
				t.Fatalf("unexpected resolved message: %#v", message)
			}
			if len(db.Diagnostics) != 1 || db.Diagnostics[0].Position.Source != path {
				t.Fatalf("source path was not retained: %#v", db.Diagnostics)
			}
		})
	}
}
