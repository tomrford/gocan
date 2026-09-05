package cdd_test

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/tomrford/gocan/cdd"
)

// Reference provenance: the 7+2 diagram is from CANdelaStudio 17 English help,
// as transcribed with this task. All other wires are independent calculations
// from its LSB-first child positioning and whole-container byte mapping rules.
// These synthetic CDD cases were not executed in CANoe or validated by its DTD.
const packedTypes = `
<IDENT id="byte"><CVALUETYPE bl="8" bo="21" enc="uns"/></IDENT>
<IDENT id="four"><CVALUETYPE bl="4" bo="12" enc="uns"/></IDENT>
<IDENT id="signedFour"><CVALUETYPE bl="4" bo="21" enc="sgn"/></IDENT>
<IDENT id="seven"><CVALUETYPE bl="7" bo="12" enc="uns"/></IDENT>
<IDENT id="nine"><CVALUETYPE bl="9" bo="21" enc="uns"/></IDENT>
<IDENT id="fourteen"><CVALUETYPE bl="14" bo="12" enc="uns"/></IDENT>
<IDENT id="signedFourteen"><CVALUETYPE bl="14" bo="21" enc="sgn"/></IDENT>
<IDENT id="two"><CVALUETYPE bl="2" bo="21" enc="uns"/></IDENT>
<TEXTTBL id="choice"><CVALUETYPE bl="4" bo="12" enc="uns"/><TEXTMAP s="3" e="3"><TEXT><TUV>On</TUV></TEXT></TEXTMAP></TEXTTBL>
<LINCOMP id="scaled"><CVALUETYPE bl="14" bo="12" enc="sgn"/><COMP f="3" div="10" o="-7"/></LINCOMP>
<LINCOMP id="exact"><CVALUETYPE bl="64" bo="12" enc="uns"/><COMP f="100" div="100" o="0"/></LINCOMP>
`

func TestPackedReferenceValues(t *testing.T) {
	for _, test := range []struct {
		name     string
		bits     int
		children string
		values   cdd.Values
		wire     []byte // big-endian enclosing value, reserved bits zero
	}{
		{"Vector diagram", 16, packedChild("seven", "A") + packedChild("two", "B"), cdd.Values{"A": uint64(0x7f), "B": uint64(0)}, []byte{0, 0x7f}},
		{"crossing boundary", 16, packedChild("seven", "A") + packedChild("nine", "B"), cdd.Values{"A": uint64(0x35), "B": uint64(0x123)}, []byte{0x91, 0xb5}},
		{"two nibbles", 8, packedChild("four", "A") + packedChild("four", "B"), cdd.Values{"A": uint64(0xa), "B": uint64(5)}, []byte{0x5a}},
		{"consecutive fourteen", 32, packedChild("fourteen", "A") + packedChild("fourteen", "B"), cdd.Values{"A": uint64(0x1234), "B": uint64(0x2345)}, []byte{0x08, 0xd1, 0x52, 0x34}},
		{"signed and choices", 8, packedChild("choice", "A") + packedChild("signedFour", "B"), cdd.Values{"A": "On", "B": int64(-2)}, []byte{0xe3}},
		{"signed fourteen minimum", 16, packedChild("signedFourteen", "A"), cdd.Values{"A": int64(-8192)}, []byte{0x20, 0}},
		{"signed fourteen maximum", 16, packedChild("signedFourteen", "A"), cdd.Values{"A": int64(8191)}, []byte{0x1f, 0xff}},
		{"scaling and explicit gap", 32, packedChild("four", "A") + `<GAPDATAOBJ bl="6"/>` + packedChild("scaled", "B"), cdd.Values{"A": uint64(5), "B": -22.0}, []byte{0, 0xff, 0x38, 5}},
		{"exact identity", 64, packedChild("exact", "A"), cdd.Values{"A": uint64(math.MaxUint64)}, bytes.Repeat([]byte{0xff}, 8)},
	} {
		for _, order := range []string{"21", "12"} {
			t.Run(test.name+"/"+order, func(t *testing.T) {
				datatype := fmt.Sprintf(`<IDENT id="container"><CVALUETYPE bl="%d" bo="%s" enc="uns"/></IDENT>`, test.bits, order)
				record := packedRecord(t, datatype, test.children, "")
				wire := append([]byte(nil), test.wire...)
				if order == "12" {
					for i, j := 0, len(wire)-1; i < j; i, j = i+1, j-1 {
						wire[i], wire[j] = wire[j], wire[i]
					}
				}
				assertCodecReference(t, record, test.values, wire)
			})
		}
	}
}

func TestPackedContainerBoundariesAndReservedBits(t *testing.T) {
	types := packedTypes + `<IDENT id="box"><CVALUETYPE bl="32" bo="21" enc="uns"/></IDENT><IDENT id="tail"><CVALUETYPE bl="16" bo="12" enc="uns"/></IDENT>`
	layout := packedChild("byte", "Header") + `<STRUCT dtref="box">` + packedChild("seven", "A") + `<GAPDATAOBJ bl="2"/>` + packedChild("nine", "B") + `</STRUCT><GAPDATAOBJ bl="8"/><STRUCT dtref="tail">` + packedChild("four", "C") + `</STRUCT>` + packedChild("byte", "Footer")
	message := testCodecMessage(t, types, layout, "")
	if message.Err != nil {
		t.Fatal(message.Err)
	}
	record := message.Record
	values := cdd.Values{"Header": uint64(0xaa), "A": uint64(0x35), "B": uint64(0x123), "C": uint64(0xa), "Footer": uint64(0x55)}
	// B starts at bit 9: 0x123*512 + 0x35 = 0x24635. The second
	// container is little-endian, followed by a separate top-level byte.
	wire := []byte{0xaa, 0, 2, 0x46, 0x35, 0, 0x0a, 0, 0x55}
	assertCodecReference(t, record, values, wire)
	reserved := []byte{0xaa, 0xff, 0xfe, 0x47, 0xb5, 0xff, 0xfa, 0xff, 0x55}
	decoded, err := record.Decode(reserved)
	if err != nil || !reflect.DeepEqual(decoded, values) {
		t.Fatalf("reserved bits affected values: %#v, %v", decoded, err)
	}
	for _, invalid := range [][]byte{wire[:len(wire)-1], append(append([]byte(nil), wire...), 0)} {
		if _, err := record.Decode(invalid); err == nil {
			t.Fatalf("accepted payload length %d", len(invalid))
		}
	}
	if record.Length != 9 || record.MaxLength != 9 || record.Fields[3].Bitfield.BitOffset != 48 {
		t.Fatalf("container boundaries lost: %#v", record)
	}
}

func TestPackedArrayByteReversal(t *testing.T) {
	for _, test := range []struct {
		name, declarations, attribute string
		wire                          []byte
	}{
		{"default mapping", "", "", []byte{0, 0, 0x91, 0xb5}},
		{"declared default", `<DEFATTS><ENUMDEF id="reverse-setting" v="1"><QUAL>ReverseBitFieldBytes</QUAL></ENUMDEF></DEFATTS>`, "", []byte{0xb5, 0x91, 0, 0}},
		{"explicit reverse", `<DEFATTS><ENUMDEF id="different-id" v="0"><QUAL>ReverseBitFieldBytes</QUAL></ENUMDEF></DEFATTS>`, `<ENUM attrref="different-id" v="2"/>`, []byte{0xb5, 0x91, 0, 0}},
		{"explicit normal", `<DEFATTS><ENUMDEF id="different-id" v="1"><QUAL>ReverseBitFieldBytes</QUAL></ENUMDEF></DEFATTS>`, `<ENUM attrref="different-id" v="0"/>`, []byte{0, 0, 0x91, 0xb5}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// bo=12 on one-byte elements must not reverse the array by itself.
			datatype := `<IDENT id="container"><CVALUETYPE bl="8" bo="12" enc="uns" qty="field" minsz="4" maxsz="4"/>` + test.attribute + `</IDENT>`
			record := packedRecord(t, datatype, packedChild("seven", "A")+packedChild("nine", "B"), test.declarations)
			assertCodecReference(t, record, cdd.Values{"A": uint64(0x35), "B": uint64(0x123)}, test.wire)
		})
	}
}

func TestPackedReversalQualifier(t *testing.T) {
	for _, test := range []struct {
		qualifier string
		wire      []byte
	}{
		{"ReverseBitfieldBytes", []byte{0xb5, 0x91}},
		{"ReverseBitFieldBytes", []byte{0xb5, 0x91}},
		{"reversebitfieldbytes", []byte{0x91, 0xb5}},
	} {
		t.Run(test.qualifier, func(t *testing.T) {
			declarations := `<DEFATTS><ENUMDEF id="reverse" v="0"><QUAL>` + test.qualifier + `</QUAL></ENUMDEF></DEFATTS>`
			datatype := `<IDENT id="container"><CVALUETYPE bl="8" bo="21" enc="uns" qty="field" minsz="2" maxsz="2"/><ENUM attrref="reverse" v="1"/></IDENT>`
			record := packedRecord(t, datatype, packedChild("seven", "A")+packedChild("nine", "B"), declarations)
			// CANdelaStudio 17's Special Attributes table uses ReverseBitfieldBytes.
			// The documented packing rules give logical 0x91b5 for these children.
			assertCodecReference(t, record, cdd.Values{"A": uint64(0x35), "B": uint64(0x123)}, test.wire)
		})
	}
}

func TestPackedEncodingWidth(t *testing.T) {
	record := packedRecord(t, `<IDENT id="container"><CVALUETYPE bl="8" bo="21" enc="uns"/></IDENT>`, packedChild("four", "A")+packedChild("signedFour", "B"), "")
	for _, values := range []cdd.Values{
		{"A": uint64(16), "B": int64(0)}, {"A": -1, "B": int64(0)},
		{"A": uint64(0), "B": int64(8)}, {"A": uint64(0), "B": int64(-9)},
		{"A": []uint8{0}, "B": int64(0)}, {"A": uint64(0)},
	} {
		if _, err := record.Encode(values); err == nil {
			t.Fatalf("accepted invalid values %#v", values)
		}
	}
	assertCodecReference(t, record, cdd.Values{"A": uint64(15), "B": int64(-8)}, []byte{0x8f})
	assertCodecReference(t, record, cdd.Values{"A": uint64(0), "B": int64(7)}, []byte{0x70})
}

func TestPackedWideArrayInteger(t *testing.T) {
	// A 64-bit value at bit 3 spans nine wire bytes in a 16-byte container.
	// This also checks that trailing container space, not the last child,
	// determines payload length.
	datatype := `<IDENT id="container"><CVALUETYPE bl="8" bo="21" enc="uns" qty="field" minsz="16" maxsz="16"/></IDENT>`
	record := packedRecord(t, datatype, `<GAPDATAOBJ bl="3"/>`+packedChild("exact", "A"), "")
	assertCodecReference(t, record, cdd.Values{"A": uint64(math.MaxUint64)}, []byte{0, 0, 0, 0, 0, 0, 0, 7, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xf8})
}

func TestPackedRejectsUnresolvedReversal(t *testing.T) {
	const declaration = `<ENUMDEF id="reverse" v="0"><QUAL>ReverseBitFieldBytes</QUAL></ENUMDEF>`
	const documentedDeclaration = `<ENUMDEF id="documented" v="0"><QUAL>ReverseBitfieldBytes</QUAL></ENUMDEF>`
	for _, test := range []struct{ name, definitions, attributes, error string }{
		{"unknown reference", declaration, `<ENUM attrref="missing" v="1"/>`, "does not resolve"},
		{"missing reference", declaration, `<ENUM v="1"/>`, "does not resolve"},
		{"duplicate values", declaration, `<ENUM attrref="reverse" v="0"/><ENUM attrref="reverse" v="1"/>`, "repeated"},
		{"duplicate IDs", declaration + declaration, "", "ambiguous"},
		{"duplicate qualifiers", declaration + `<ENUMDEF id="another" v="1"><QUAL>ReverseBitFieldBytes</QUAL></ENUMDEF>`, "", "ambiguous"},
		{"duplicate aliases", declaration + documentedDeclaration, "", "ambiguous"},
		{"duplicate aliases reversed", documentedDeclaration + declaration, "", "ambiguous"},
		{"wrong definition", `<UNSDEF id="reverse" v="1"><QUAL>ReverseBitFieldBytes</QUAL></UNSDEF>`, "", "not an enumeration"},
		{"wrong value type", declaration, `<UNS attrref="reverse" v="1"/>`, "requires an ENUM"},
		{"missing default", `<ENUMDEF id="reverse"><QUAL>ReverseBitFieldBytes</QUAL></ENUMDEF>`, "", "invalid ReverseBitFieldBytes"},
		{"missing value", declaration, `<ENUM attrref="reverse"/>`, "invalid ReverseBitFieldBytes"},
		{"malformed value", declaration, `<ENUM attrref="reverse" v="NaN"/>`, "invalid ReverseBitFieldBytes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			datatype := `<IDENT id="container"><CVALUETYPE bl="8" bo="21" enc="uns" qty="field" minsz="2" maxsz="2"/>` + test.attributes + `</IDENT>`
			message := testCodecMessage(t, packedTypes+datatype, `<STRUCT dtref="container">`+packedChild("seven", "A")+`</STRUCT>`, `<DEFATTS>`+test.definitions+`</DEFATTS>`)
			if message.Err == nil || !strings.Contains(message.Err.Error(), test.error) {
				t.Fatalf("error = %v, want %q", message.Err, test.error)
			}
		})
	}
}

func TestPackedUnsupportedLayouts(t *testing.T) {
	const box = `<IDENT id="container"><CVALUETYPE bl="16" bo="21" enc="uns"/></IDENT>`
	const child = `<DATAOBJ dtref="seven"><QUAL>A</QUAL></DATAOBJ>`
	for _, test := range []struct{ name, datatype, layout, declarations, error string }{
		{"missing container", box, `<STRUCT>` + child + `</STRUCT>`, "", "unknown datatype"},
		{"unknown container", box, `<STRUCT dtref="missing">` + child + `</STRUCT>`, "", "unknown datatype"},
		{"overfull", box, `<STRUCT dtref="container">` + packedChild("nine", "A") + packedChild("nine", "B") + `</STRUCT>`, "", "exceed"},
		{"oversized gap", box, `<STRUCT dtref="container"><GAPDATAOBJ bl="17"/></STRUCT>`, "", "exceed"},
		{"bad gap", box, `<STRUCT dtref="container"><GAPDATAOBJ bl="bad"/></STRUCT>`, "", "invalid bit length"},
		{"nested", box, `<STRUCT dtref="container"><STRUCT dtref="container">` + child + `</STRUCT></STRUCT>`, "", "nested"},
		{"unaligned top-level", box, child, "", "not byte-aligned"},
		{"unaligned gap", box, `<GAPDATAOBJ bl="1"/>`, "", "not byte-aligned"},
		{"unaligned container", `<IDENT id="container"><CVALUETYPE bl="14" bo="21" enc="uns"/></IDENT>`, `<STRUCT dtref="container">` + child + `</STRUCT>`, "", "byte-aligned"},
		{"unicode array", `<IDENT id="container"><CVALUETYPE bl="16" bo="21" enc="utf" qty="field" minsz="2" maxsz="2"/></IDENT>`, `<STRUCT dtref="container">` + child + `</STRUCT>`, "", "arrays other than"},
		{"variable container", `<IDENT id="container"><CVALUETYPE bl="8" bo="21" enc="uns" qty="field" minsz="1" maxsz="4"/></IDENT>`, `<STRUCT dtref="container">` + child + `</STRUCT>`, "", "fixed"},
		{"float child", box + `<IDENT id="float"><CVALUETYPE bl="32" bo="21" enc="flt"/></IDENT>`, `<STRUCT dtref="container">` + packedChild("float", "A") + `</STRUCT>`, "", "atomic integer"},
		{"array child", box + `<IDENT id="array"><CVALUETYPE bl="1" bo="21" enc="uns" qty="field" minsz="1" maxsz="1"/></IDENT>`, `<STRUCT dtref="container">` + packedChild("array", "A") + `</STRUCT>`, "", "atomic integer"},
		{"ambiguous byte order", `<IDENT id="container"><CVALUETYPE bl="16" bo="21" enc="uns"/><CVALUETYPE bl="16" bo="12" enc="uns"/></IDENT>`, `<STRUCT dtref="container">` + child + `</STRUCT>`, "", "ambiguous coded type"},
		{"ambiguous conversion", box + `<LINCOMP id="bad"><CVALUETYPE bl="7" bo="21" enc="uns"/><COMP f="1" o="0"/><COMP f="2" o="0"/></LINCOMP>`, `<STRUCT dtref="container">` + packedChild("bad", "A") + `</STRUCT>`, "", "ambiguous coded type"},
		{"missing conversion", box + `<LINCOMP id="bad"><CVALUETYPE bl="7" bo="21" enc="uns"/></LINCOMP>`, `<STRUCT dtref="container">` + packedChild("bad", "A") + `</STRUCT>`, "", "has no COMP"},
		{"empty raw interval", box + `<LINCOMP id="bad"><CVALUETYPE bl="7" bo="21" enc="uns"/><COMP f="1" o="0" s="128"/></LINCOMP>`, `<STRUCT dtref="container">` + packedChild("bad", "A") + `</STRUCT>`, "", "no representable raw values"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := testCodecMessage(t, packedTypes+test.datatype, test.layout, test.declarations)
			err := message.Err
			if err == nil && message.Record != nil {
				err = message.Record.CodecError()
			}
			if err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("error = %v, want %q", err, test.error)
			}
		})
	}
}

func packedChild(datatype, name string) string {
	return fmt.Sprintf(`<DATAOBJ dtref="%s"><QUAL>%s</QUAL></DATAOBJ>`, datatype, name)
}

func packedRecord(t *testing.T, datatype, children, declarations string) *cdd.Record {
	t.Helper()
	message := testCodecMessage(t, packedTypes+datatype, `<STRUCT dtref="container">`+children+`</STRUCT>`, declarations)
	if message.Err != nil {
		t.Fatal(message.Err)
	}
	if err := message.Record.CodecError(); err != nil {
		t.Fatal(err)
	}
	return message.Record
}

func assertCodecReference(t *testing.T, record *cdd.Record, values cdd.Values, wire []byte) {
	t.Helper()
	t.Run("decode", func(t *testing.T) {
		decoded, err := record.Decode(wire)
		if err != nil || !reflect.DeepEqual(decoded, values) {
			t.Fatalf("decoded %x = %#v, %v; want %#v", wire, decoded, err, values)
		}
	})
	t.Run("encode", func(t *testing.T) {
		encoded, err := record.Encode(values)
		if err != nil || !bytes.Equal(encoded, wire) {
			t.Fatalf("encoded %#v = %x, %v; want %x", values, encoded, err, wire)
		}
	})
}
