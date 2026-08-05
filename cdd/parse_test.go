package cdd_test

import (
	"strings"
	"testing"

	"github.com/tomrford/gocan/cdd"
)

func TestParseResolvedDIDCatalog(t *testing.T) {
	database, err := cdd.Parse("catalog.cdd", fixtureCDD())
	if err != nil {
		t.Fatal(err)
	}
	if len(database.DIDs) != 1 {
		t.Fatalf("got %d DIDs, want 1", len(database.DIDs))
	}

	did, ok := database.DIDByName("CaféStatus")
	if !ok {
		t.Fatal("DID lookup by decoded ISO-8859-1 name failed")
	}
	if did.Identifier != 0xf190 || did.Length != 5 {
		t.Fatalf("got DID %#04x with length %d, want 0xf190 with length 5", did.Identifier, did.Length)
	}
	if byIdentifier, ok := database.DIDByIdentifier(0xf190); !ok || byIdentifier != did {
		t.Fatal("DID lookup by identifier did not return the catalog entry")
	}
	if len(did.Fields) != 2 {
		t.Fatalf("got %d fields, want 2", len(did.Fields))
	}

	signed := did.Fields[0]
	if signed.Name != "Temperature" || signed.BitOffset != 0 || signed.BitLength != 8 || signed.Encoding != cdd.EncodingSigned || signed.ByteOrder != cdd.ByteOrderLittle {
		t.Fatalf("unexpected signed field: %#v", signed)
	}
	if signed.Conversion == nil || signed.Conversion.Scale != 0.5 || signed.Conversion.Offset != -40 || signed.Unit != "degC" {
		t.Fatalf("unexpected signed field conversion: %#v", signed)
	}
	if len(signed.Choices) != 1 || signed.Choices[0].Value != -1 || signed.Choices[0].Label != "Unavailable" {
		t.Fatalf("unexpected signed field choices: %#v", signed.Choices)
	}

	text := did.Fields[1]
	if text.Name != "Code" || text.BitOffset != 8 || text.BitLength != 32 || text.Encoding != cdd.EncodingASCII || text.ByteOrder != cdd.ByteOrderBig {
		t.Fatalf("unexpected shared text field: %#v", text)
	}
}

func TestParseSelectedDIDTrustBoundaries(t *testing.T) {
	t.Run("broken shared data reference", func(t *testing.T) {
		source := strings.Replace(string(fixtureCDD()), `didRef="sharedData"`, `didRef="missing"`, 1)
		if _, err := cdd.Parse("broken.cdd", []byte(source)); err == nil || !strings.Contains(err.Error(), "does not resolve") {
			t.Fatalf("got error %v, want unresolved DIDDATAREF", err)
		}
	})

	t.Run("duplicate identifier", func(t *testing.T) {
		source := strings.Replace(string(fixtureCDD()), "</DIAGCLASS>", `
          <DIAGINST tmplref="didClass">
            <QUAL>Duplicate</QUAL>
            <SERVICE tmplref="readTemplate"/>
            <STATICVALUE shstaticref="didStatic" v="61840"/>
            <SIMPLECOMPCONT><DATAOBJ dtref="signed8"><QUAL>Value</QUAL></DATAOBJ></SIMPLECOMPCONT>
          </DIAGINST>
        </DIAGCLASS>`, 1)
		if _, err := cdd.Parse("duplicate.cdd", []byte(source)); err == nil || !strings.Contains(err.Error(), "use identifier") {
			t.Fatalf("got error %v, want duplicate identifier", err)
		}
	})

	t.Run("layout overflow", func(t *testing.T) {
		source := strings.Replace(string(fixtureCDD()), `bl="8" bo="12" enc="sgn"`, `bl="4294967295" bo="12" enc="sgn"`, 1)
		if _, err := cdd.Parse("overflow.cdd", []byte(source)); err == nil || !strings.Contains(err.Error(), "exceeds the supported bit length") {
			t.Fatalf("got error %v, want layout overflow", err)
		}
	})
}

func fixtureCDD() []byte {
	source := `<?xml version="1.0" encoding="iso-8859-1"?>
<CANDELA>
  <ECUDOC>
    <DATATYPES>
      <IDENT id="identifier16"><QUAL>Identifier</QUAL><CVALUETYPE bl="16" bo="21" enc="uns" qty="atom"/></IDENT>
      <IDENT id="signed8">
        <QUAL>Signed</QUAL><CVALUETYPE bl="8" bo="12" enc="sgn" qty="atom"/>
        <PVALUETYPE><UNIT>degC</UNIT></PVALUETYPE><COMP f="0.5" o="-40"/>
        <TEXTMAP s="(-1)" e="(-1)"><TEXT><TUV>Unavailable</TUV></TEXT></TEXTMAP>
      </IDENT>
      <IDENT id="ascii4"><QUAL>ASCII</QUAL><CVALUETYPE bl="8" bo="21" enc="asc" qty="field" minsz="4" maxsz="4"/></IDENT>
    </DATATYPES>
    <DIDS>
      <DID id="sharedData"><STRUCTURE><DATAOBJ dtref="ascii4"><QUAL>Code</QUAL></DATAOBJ></STRUCTURE></DID>
    </DIDS>
    <PROTOCOLSERVICES>
      <PROTOCOLSERVICE id="readDID">
		<REQ><CONSTCOMP spec="sid" v="34"/><STATICCOMP id="didComponent" spec="id" dtref="identifier16"/></REQ>
		<POS><SIMPLEPROXYCOMP id="readData" dest="data"/></POS>
      </PROTOCOLSERVICE>
      <PROTOCOLSERVICE id="sessionControl"><REQ><CONSTCOMP spec="sid" v="16"/></REQ></PROTOCOLSERVICE>
    </PROTOCOLSERVICES>
    <DCLTMPLS>
      <DCLTMPL id="didClass">
        <DCLSRVTMPL id="readTemplate" tmplref="readDID"/>
        <SHSTATIC id="didStatic" spec="id"><STATICCOMPREF idref="didComponent"/></SHSTATIC>
		<SHPROXY dest="data" spec="didDataReference"><PROXYCOMPREF idref="readData"/></SHPROXY>
      </DCLTMPL>
      <DCLTMPL id="sessionClass">
        <DCLSRVTMPL id="sessionTemplate" tmplref="sessionControl"/>
        <SHSTATIC id="sessionStatic" spec="sub"/>
      </DCLTMPL>
	  <DCLTMPL id="borrowerClass">
		<SHSTATIC id="borrowerStatic" spec="id"><STATICCOMPREF idref="didComponent"/></SHSTATIC>
		<SHPROXY dest="data" spec="didDataReference"><PROXYCOMPREF idref="readData"/></SHPROXY>
	  </DCLTMPL>
    </DCLTMPLS>
    <ECU>
      <VAR>
        <DIAGCLASS>
          <DIAGINST tmplref="sessionClass">
            <QUAL>ProgrammingSession</QUAL><SERVICE tmplref="sessionTemplate"/><STATICVALUE shstaticref="sessionStatic" v="2"/>
          </DIAGINST>
        </DIAGCLASS>
		<DIAGCLASS>
		  <DIAGINST tmplref="borrowerClass">
			<QUAL>BorrowedService</QUAL><SERVICE tmplref="readTemplate"/><STATICVALUE shstaticref="borrowerStatic" v="61841"/>
			<SIMPLECOMPCONT><DATAOBJ dtref="signed8"><QUAL>Value</QUAL></DATAOBJ></SIMPLECOMPCONT>
		  </DIAGINST>
		</DIAGCLASS>
        <DIAGCLASS>
          <DIAGINST tmplref="didClass">
            <QUAL>Caf` + string([]byte{0xe9}) + `Status</QUAL>
            <SERVICE tmplref="readTemplate"/>
            <STATICVALUE shstaticref="didStatic" v="61840"/>
            <SIMPLECOMPCONT>
              <DATAOBJ dtref="signed8"><QUAL>Temperature</QUAL></DATAOBJ>
              <DIDDATAREF didRef="sharedData"/>
            </SIMPLECOMPCONT>
          </DIAGINST>
        </DIAGCLASS>
      </VAR>
      <VAR><DIAGCLASS><DIAGINST tmplref="didClass"><SERVICE tmplref="readTemplate"/></DIAGINST></DIAGCLASS></VAR>
    </ECU>
    <ECU><VAR><DIAGCLASS><DIAGINST tmplref="didClass"><SERVICE tmplref="readTemplate"/></DIAGINST></DIAGCLASS></VAR></ECU>
  </ECUDOC>
</CANDELA>`
	return []byte(source)
}
