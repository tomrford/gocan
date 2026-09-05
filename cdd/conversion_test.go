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

// These cases come from the CANdelaStudio 9.1/17 English help equation and
// independent arithmetic supplied with the task, not CANoe execution or
// cantools. Inputs in each direction are literal, independently specified.
func TestLINCOMPReferenceValues(t *testing.T) {
	for _, test := range []struct {
		name, comp, encoding string
		bits                 int
		physical             any
		wire                 []byte
	}{
		{"hundredths", `f="1" div="100" o="0"`, "uns", 16, 5.02, []byte{0x01, 0xf6}},
		{"tenths119", `f="1" div="10" o="0"`, "uns", 16, 11.9, []byte{0, 0x77}},
		{"tenths117", `f="1" div="10" o="0"`, "uns", 16, 11.7, []byte{0, 0x75}},
		{"offset", `f="3" div="10" o="-7"`, "sgn", 16, 8.0, []byte{0, 0x32}},
		{"raw zero", `f="3" div="10" o="-7"`, "sgn", 16, -7.0, []byte{0, 0}},
		{"negative raw", `f="3" div="10" o="-7"`, "sgn", 16, -22.0, []byte{0xff, 0xce}},
		{"default divisor", `f="3" o="-7"`, "sgn", 16, 143.0, []byte{0, 0x32}},
		{"negative divisor", `f="3" div="-10" o="-7"`, "sgn", 16, -22.0, []byte{0, 0x32}},
		{"unsigned maximum", `f="1" div="100" o="0"`, "uns", 16, 655.35, []byte{0xff, 0xff}},
		{"signed minimum", `f="1" div="10" o="0"`, "sgn", 16, -3276.8, []byte{0x80, 0}},
		{"signed maximum", `f="1" div="10" o="0"`, "sgn", 16, 3276.7, []byte{0x7f, 0xff}},
		{"exact unsigned", `f="100" div="100" o="0" s="0" e="18446744073709551615"`, "uns", 64, uint64(math.MaxUint64), bytes.Repeat([]byte{0xff}, 8)},
		{"exact positive", `f="10" div="10" o="0"`, "sgn", 64, int64(9007199254740993), []byte{0, 0x20, 0, 0, 0, 0, 0, 1}},
		{"exact negative", `f="10" div="10" o="0"`, "sgn", 64, int64(-9007199254740993), []byte{0xff, 0xdf, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
		{"exact signed minimum", `f="1" div="1" o="0" s="-9223372036854775808"`, "sgn", 64, int64(math.MinInt64), []byte{0x80, 0, 0, 0, 0, 0, 0, 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := conversionRecord(t, test.comp, test.encoding, test.bits)
			t.Run("decode", func(t *testing.T) {
				values, err := record.Decode(test.wire)
				if err != nil {
					t.Fatal(err)
				}
				if want, floating := test.physical.(float64); floating {
					got, ok := values["Value"].(float64)
					if !ok || math.Abs(got-want) > 1e-10 {
						t.Fatalf("decoded %x = %#v, want %v", test.wire, values, want)
					}
				} else if !reflect.DeepEqual(values["Value"], test.physical) {
					t.Fatalf("decoded %x = %#v, want %#v", test.wire, values, test.physical)
				}
			})
			t.Run("encode", func(t *testing.T) {
				wire, err := record.Encode(cdd.Values{"Value": test.physical})
				if err != nil || !bytes.Equal(wire, test.wire) {
					t.Fatalf("encoded %v = %x, %v; want %x", test.physical, wire, err, test.wire)
				}
			})
		})
	}
}

func TestLINCOMPRejectsUnsupportedDefinitions(t *testing.T) {
	for _, test := range []struct{ comp, error string }{
		{`f="1" o="0" div=""`, "invalid div"},
		{`f="1" o="0" div="bad"`, "invalid div"},
		{`f="1" o="0" div="0"`, "zero divisor"},
		{`f="1" o="0" div="-0"`, "zero divisor"},
		{`f="1" o="0" div="NaN"`, "invalid div"},
		{`f="1" o="0" div="Inf"`, "invalid div"},
		{`f="1" o="0" div="-Inf"`, "invalid div"},
		{`f="1" o="0" div="1e999"`, "invalid div"},
		{`f="1e308" o="0" div="1e-308"`, "not representable"},
		{`f="1e-308" o="0" div="1e308"`, "not representable"},
		{`o="0"`, "without f is unsupported"},
		{`f="1"`, "without o is unsupported"},
		{`f="NaN" o="0"`, "invalid f"},
		{`f="1" o="Inf"`, "invalid o"},
		{`f="0" o="7"`, "explicit inverse value is unsupported"},
		{`f="1" o="0" s="5" e="4"`, "limits are reversed"},
		{`f="1" o="0" s="bad"`, "invalid raw limit"},
		{`f="1" o="0" s="32768"`, "no representable raw values"},
		{`f="1" o="0" e="-32769"`, "no representable raw values"},
	} {
		t.Run(test.comp, func(t *testing.T) {
			message := testCodecMessage(t, fmt.Sprintf(`<LINCOMP id="value"><CVALUETYPE bl="16" bo="21" enc="sgn"/><COMP %s/></LINCOMP>`, test.comp), `<DATAOBJ dtref="value"><QUAL>Value</QUAL></DATAOBJ>`, "")
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

func TestLINCOMPRawLimitsAndRepresentability(t *testing.T) {
	record := conversionRecord(t, `f="3" div="10" o="-7" s="-50" e="50"`, "sgn", 16)
	for _, physical := range []float64{-22, 8} {
		if _, err := record.Encode(cdd.Values{"Value": physical}); err != nil {
			t.Fatal(err)
		}
	}
	for _, physical := range []float64{-22.3, 8.3, math.NaN(), math.Inf(1)} {
		if _, err := record.Encode(cdd.Values{"Value": physical}); err == nil {
			t.Fatalf("encoded invalid %v", physical)
		}
	}
	for _, wire := range [][]byte{{0xff, 0xcd}, {0, 0x33}} {
		if _, err := record.Decode(wire); err == nil {
			t.Fatalf("decoded out-of-range %x", wire)
		}
	}
	for _, encoding := range []string{"uns", "sgn"} {
		record := conversionRecord(t, `f="1" div="10" o="0"`, encoding, 16)
		for _, physical := range []float64{-3276.9, 6553.6} {
			if _, err := record.Encode(cdd.Values{"Value": physical}); err == nil {
				t.Fatalf("encoded out-of-width %v as %s", physical, encoding)
			}
		}
	}
	record = conversionRecord(t, `f="1" div="10" o="0"`, "uns", 64)
	if _, err := record.Encode(cdd.Values{"Value": 1e16}); err == nil {
		t.Fatal("accepted imprecise converted integer")
	}
}

func conversionRecord(t *testing.T, comp, encoding string, bits int) *cdd.Record {
	t.Helper()
	message := testCodecMessage(t, fmt.Sprintf(`<LINCOMP id="value"><CVALUETYPE bl="%d" bo="21" enc="%s"/><COMP %s/></LINCOMP>`, bits, encoding, comp), `<DATAOBJ dtref="value"><QUAL>Value</QUAL></DATAOBJ>`, "")
	if message.Err != nil {
		t.Fatal(message.Err)
	}
	if err := message.Record.CodecError(); err != nil {
		t.Fatal(err)
	}
	return message.Record
}

// A synthetic service envelope keeps codec fixtures independent of unrelated
// catalog records. The version identifies the subset under test, not a claim
// of validation against the unavailable 10.0.108 DTD.
func testCodecMessage(t *testing.T, datatypes, layout, declarations string) *cdd.Message {
	t.Helper()
	source := fmt.Sprintf(`<CANDELA dtdvers="10.0.108"><ECUDOC>
%s<DATATYPES>%s</DATATYPES>
<PROTOCOLSERVICES><PROTOCOLSERVICE id="read"><REQ><CONSTCOMP spec="sid" bl="8" v="34"/><STATICCOMP id="identifier" spec="id" bl="16"/></REQ><POS><SIMPLEPROXYCOMP id="data" dest="data"/></POS></PROTOCOLSERVICE></PROTOCOLSERVICES>
<DCLTMPLS><DCLTMPL id="class"><DCLSRVTMPL id="service" tmplref="read"/><SHSTATIC id="id"><STATICCOMPREF idref="identifier"/></SHSTATIC><SHPROXY id="proxy" dest="data"><PROXYCOMPREF idref="data"/></SHPROXY></DCLTMPL></DCLTMPLS>
<ECU><VAR><DIAGCLASS tmplref="class"><DIAGINST tmplref="class"><QUAL>Sample</QUAL><SERVICE tmplref="service"/><STATICVALUE shstaticref="id" v="61840"/><SIMPLECOMPCONT shproxyref="proxy">%s</SIMPLECOMPCONT></DIAGINST></DIAGCLASS></VAR></ECU>
</ECUDOC></CANDELA>`, declarations, datatypes, layout)
	db, err := parseCatalog("codec-reference.cdd", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	did, ok := db.DIDByName("Sample")
	if !ok || len(did.Read) != 1 {
		t.Fatalf("missing sample service: %#v; diagnostics %#v", did, db.Diagnostics)
	}
	return did.Read[0].PositiveResponse
}
