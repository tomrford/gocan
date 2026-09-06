package dbc_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomrford/gocan/dbc"
)

func TestExport(t *testing.T) {
	for _, name := range []string{"core", "codec", "multiplex_j1939"} {
		t.Run(name, func(t *testing.T) {
			db, err := dbc.ParseFile("testdata/" + name + ".dbc")
			if err != nil {
				t.Fatal(err)
			}
			text, err := db.MarshalText()
			if err != nil {
				t.Fatal(err)
			}
			again, err := db.MarshalText()
			if err != nil || !bytes.Equal(text, again) {
				t.Fatal("export is not deterministic", err)
			}
			if strings.Contains(string(text), "CM_") {
				t.Fatal("invented discarded comments")
			}
			checkCantools(t, name, text)
		})
	}
}

func TestExportEditedAndUnsupportedModels(t *testing.T) {
	// Numeric enum values outside the label table and undeclared attributes
	// survive loading and must remain exportable without invented definitions.
	odd, err := dbc.Parse("", "BA_DEF_ \"E\" ENUM \"A\";\nBA_DEF_DEF_ \"E\" 9;\nBA_ \"E\" -2;\nBA_ \"Extra\" 1.0;\n")
	if err != nil {
		t.Fatal(err)
	}
	text, err := odd.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`BA_DEF_DEF_ "E" 9;`, `BA_ "E" -2;`, `BA_ "Extra" 1.0;`} {
		if !strings.Contains(string(text), want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
	odd.Attributes["bad\x00name"] = dbc.AttributeValue{Kind: dbc.AttributeKindInteger, Integer: 1}
	if text, err := odd.MarshalText(); err == nil || text != nil {
		t.Fatal("NUL in attribute name accepted")
	}
	db, err := dbc.Parse("", "BO_ 42 Sample: 1 ECU\n SG_ Value : 0|8@1+ (1,0) [0|255] \"\" ECU\n")
	if err != nil {
		t.Fatal(err)
	}
	db.Messages[0].Signals[0].Factor = 0.5
	db.Messages[0].Signals[0].Unit = `a "quoted" unit\path`
	text, err = db.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "(0.5,0)") || !strings.Contains(string(text), `\"quoted\"`) {
		t.Fatalf("edited fields missing: %s", text)
	}
	checkCantools(t, "edited", text)
	db.Messages[0].Signals[0].Unit = `literal\n`
	if text, err := db.MarshalText(); err == nil || text != nil {
		t.Fatal("ambiguous backslash escape accepted")
	}
	db.Messages[0].Signals[0].Unit = ""
	// An FD marker on a short programmatic message needs matching attributes;
	// otherwise an independent DBC reader would infer classical CAN.
	db.Messages[0].Format = dbc.FrameFormatStandardCANFD
	if text, err := db.MarshalText(); err == nil || text != nil {
		t.Fatal("silently lost FD format")
	}
	db.Messages[0].Format = dbc.FrameFormatStandardCAN
	db.Messages[0].Signals[0].ByteOrder = 9
	if text, err := db.MarshalText(); err == nil || text != nil {
		t.Fatal("accepted invalid byte order")
	}
	db.Messages[0].Signals[0].ByteOrder = dbc.ByteOrderLittleEndian
	db.Messages[0].Signals[0].Multiplex = &dbc.MultiplexCondition{Selector: "missing"}
	if text, err := db.MarshalText(); err == nil || text != nil {
		t.Fatal("accepted unrepresentable multiplexing")
	}
}

func checkCantools(t *testing.T, name string, text []byte) {
	t.Helper()
	if python := os.Getenv("GOCAN_INTEROP_PYTHON"); python != "" {
		file := filepath.Join(t.TempDir(), name+".dbc")
		if err := os.WriteFile(file, text, 0600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command(python, "testdata/check_export.py", file).CombinedOutput()
		if err != nil {
			t.Fatalf("cantools: %v\n%s", err, output)
		}
	}
}
