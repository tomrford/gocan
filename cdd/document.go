package cdd

import (
	"fmt"
	"os"
)

// Document is a decoded CDD document. Parse reads XML and indexes references
// once; Select resolves an ECU/variant without re-reading the XML. Treat a
// Document and all returned catalog objects as read-only.
type Document struct {
	Metadata
	Version string
	ECUs    []ECU

	resolver *resolver
	ecus     []*element
}

// ECU is one ECU choice, with its direct VAR children in source order.
// Index is one-based within Document.ECUs.
type ECU struct {
	Metadata
	Index    int
	Variants []Variant
}

// Variant is one variant choice. Index is one-based within its ECU.
// Base retains the literal base attribute; it does not select the variant or
// cause content from another variant to be inherited.
type Variant struct {
	Metadata
	Index int
	Base  string
}

// Selection identifies one-based ECU and variant positions. A zero selects
// automatically only when there is exactly one choice at that level. Source
// indexes allow selection even when names and XML identities are missing or
// repeated. They are local to this document, not persistent ECU identifiers.
type Selection struct {
	ECU     int
	Variant int
}

// Parse decodes a CDD document and exposes its ECU/variant choices. Select
// resolves a catalog. Unsupported service layouts do not prevent selection.
func Parse(name string, source []byte) (*Document, error) {
	root, err := decodeXML(name, source)
	if err != nil {
		return nil, err
	}
	if root.name != "CANDELA" {
		return nil, sourceError(name, "root element is %s, want CANDELA", root.name)
	}
	ecuDoc := root.child("ECUDOC")
	if ecuDoc == nil {
		return nil, sourceError(name, "ECUDOC is missing")
	}
	document := &Document{
		Metadata: metadata(ecuDoc), Version: root.attr("dtdvers"),
		resolver: newResolver(name, ecuDoc), ecus: ecuDoc.childrenNamed("ECU"),
	}
	for index, node := range document.ecus {
		ecu := ECU{Metadata: metadata(node), Index: index + 1}
		for index, variant := range node.childrenNamed("VAR") {
			ecu.Variants = append(ecu.Variants, Variant{Metadata: metadata(variant), Index: index + 1, Base: variant.attr("base")})
		}
		document.ECUs = append(document.ECUs, ecu)
	}
	return document, nil
}

// ParseFile reads a CDD document using its declared XML character encoding.
func ParseFile(path string) (*Document, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, source)
}

// Select resolves only the selected direct VAR content. It neither merges
// variants nor infers inheritance. Selection{} succeeds only for a document
// with exactly one ECU and one variant. Calls can run concurrently and return
// independent catalogs, including their codecs and diagnostics.
func (document *Document) Select(selection Selection) (*Database, error) {
	if document == nil || document.resolver == nil {
		return nil, fmt.Errorf("CDD document has not been parsed")
	}
	ecuIndex, err := selectIndex("ECU", selection.ECU, len(document.ecus))
	if err != nil {
		return nil, sourceError(document.resolver.name, "%v", err)
	}
	variants := document.ecus[ecuIndex].childrenNamed("VAR")
	variantIndex, err := selectIndex("variant", selection.Variant, len(variants))
	if err != nil {
		return nil, sourceError(document.resolver.name, "ECU %d: %v", ecuIndex+1, err)
	}
	// Reference indexes and XML are immutable; state resolution is per catalog.
	resolver := *document.resolver
	return resolver.resolve(document.ecus[ecuIndex], variants[variantIndex], Selection{ECU: ecuIndex + 1, Variant: variantIndex + 1}), nil
}

func selectIndex(kind string, index, count int) (int, error) {
	if count == 0 {
		return 0, fmt.Errorf("no %s choices are declared", kind)
	}
	if index == 0 && count == 1 {
		return 0, nil
	}
	if index == 0 {
		return 0, fmt.Errorf("%d %s choices; select one explicitly", count, kind)
	}
	if index < 1 || index > count {
		return 0, fmt.Errorf("%s index %d is outside 1 through %d", kind, index, count)
	}
	return index - 1, nil
}
