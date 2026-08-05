package cdd

import (
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
)

const maxRecordNesting = 256

// Parse parses the supported CDD subset into a resolved UDS DID catalog. It
// includes the fixed-layout records of diagnostic instances whose class
// template binds ReadDataByIdentifier or WriteDataByIdentifier. Parse uses the
// first ECU and that ECU's first direct VAR; other ECUs and variants are not
// inspected.
//
// A data identifier whose record falls outside the subset is reported in
// Database.Diagnostics rather than failing the document, because a document
// mixes records this package can lay out with records it cannot: variable-length
// fields followed by further data, alternative layouts selected by a union, and
// the record data types behind fault memory. Parse fails only when the document
// itself is unusable. A document that describes no data identifiers, such as one
// written against KWP2000 local identifiers, yields an empty catalog and no
// error.
func Parse(name string, source []byte) (*Database, error) {
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

	resolver := newResolver(name, ecuDoc)
	database, err := resolver.resolve()
	if err != nil {
		return nil, err
	}
	return database, nil
}

// ParseFile reads and parses a CDD file. The XML declaration states the
// character encoding, so the bytes are passed through unchanged.
func ParseFile(path string) (*Database, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, source)
}

type resolver struct {
	name      string
	ecuDoc    *element
	byID      map[string]*element
	datatypes map[string]*element
}

func newResolver(name string, ecuDoc *element) *resolver {
	resolver := &resolver{
		name:      name,
		ecuDoc:    ecuDoc,
		byID:      make(map[string]*element),
		datatypes: make(map[string]*element),
	}
	resolver.index(ecuDoc)
	if datatypes := ecuDoc.child("DATATYPES"); datatypes != nil {
		for _, datatype := range datatypes.children {
			if id := datatype.attr("id"); id != "" {
				resolver.datatypes[id] = datatype
			}
		}
	}
	return resolver
}

func (resolver *resolver) index(node *element) {
	if id := node.attr("id"); id != "" {
		// CANdela IDs should be unique. Retaining the last definition keeps
		// malformed documents deterministic and matches cantools.
		resolver.byID[id] = node
	}
	for _, child := range node.children {
		resolver.index(child)
	}
}

func (resolver *resolver) resolve() (*Database, error) {
	ecu := resolver.ecuDoc.child("ECU")
	if ecu == nil {
		return nil, sourceError(resolver.name, "ECU is missing")
	}
	variant := ecu.child("VAR")
	if variant == nil {
		return nil, sourceError(resolver.name, "first ECU has no VAR")
	}

	database := &Database{
		didsByName:       make(map[string]int),
		didsByIdentifier: make(map[uint16]int),
	}
	for _, class := range variant.childrenNamed("DIAGCLASS") {
		for _, instance := range class.childrenNamed("DIAGINST") {
			did, selected, err := resolver.resolveDID(instance)
			if !selected {
				continue
			}
			if err != nil {
				database.drop(instance.childText("QUAL"), err)
				continue
			}
			if previous, exists := database.didsByName[did.Name]; exists {
				database.drop(did.Name, sourceError(resolver.name, "name repeats DID %#04x", database.DIDs[previous].Identifier))
				continue
			}
			if previous, exists := database.didsByIdentifier[did.Identifier]; exists {
				database.drop(did.Name, sourceError(resolver.name, "identifier %#04x is already used by DID %q", did.Identifier, database.DIDs[previous].Name))
				continue
			}
			database.didsByName[did.Name] = len(database.DIDs)
			database.didsByIdentifier[did.Identifier] = len(database.DIDs)
			database.DIDs = append(database.DIDs, did)
		}
	}
	return database, nil
}

func (database *Database) drop(name string, err error) {
	database.Diagnostics = append(database.Diagnostics, Diagnostic{Name: name, Message: err.Error()})
}

// resolveDID resolves one diagnostic instance. The boolean reports whether the
// instance is a data identifier at all; an unselected instance is some other
// diagnostic class and is not a defect. A selected instance that cannot be
// resolved returns an error for the caller to record as a diagnostic.
func (resolver *resolver) resolveDID(instance *element) (DID, bool, error) {
	classTemplate := resolver.byID[instance.attr("tmplref")]
	if classTemplate == nil || classTemplate.name != "DCLTMPL" {
		return DID{}, false, nil
	}
	identifiers, data, selected := resolver.didServiceComponents(instance, classTemplate)
	if !selected {
		return DID{}, false, nil
	}

	identifierStatic, err := resolver.identifierStatic(classTemplate, identifiers)
	if err != nil {
		return DID{}, true, err
	}
	dataProxies, err := resolver.dataProxies(classTemplate, data)
	if err != nil {
		return DID{}, true, err
	}

	name := instance.childText("QUAL")
	if name == "" {
		return DID{}, true, sourceError(resolver.name, "data identifier has no QUAL")
	}
	var identifierValue *element
	for _, value := range instance.childrenNamed("STATICVALUE") {
		if value.attr("shstaticref") == identifierStatic.attr("id") {
			identifierValue = value
			break
		}
	}
	if identifierValue == nil {
		return DID{}, true, sourceError(resolver.name, "DID %q has no data identifier value", name)
	}
	identifier64, err := strconv.ParseUint(identifierValue.attr("v"), 10, 16)
	if err != nil {
		return DID{}, true, sourceError(resolver.name, "DID %q has invalid data identifier %q", name, identifierValue.attr("v"))
	}

	fields, bitLength, maxBitLength, err := resolver.resolveDataRecord(instance, dataProxies)
	if err != nil {
		return DID{}, true, fmt.Errorf("DID %q: %w", name, err)
	}
	return DID{
		Name:       name,
		Identifier: uint16(identifier64),
		Length:     uint32((uint64(bitLength) + 7) / 8),
		MaxLength:  uint32((uint64(maxBitLength) + 7) / 8),
		Fields:     fields,
	}, true, nil
}

// didServiceComponents returns the identifier and data components used by the
// ReadDataByIdentifier or WriteDataByIdentifier services enabled for this
// instance. The service identifier is the authority here; DCLTMPL@cls is an
// authoring hint that CANdela versions spell differently.
func (resolver *resolver) didServiceComponents(instance, classTemplate *element) (map[string]struct{}, map[string]struct{}, bool) {
	identifiers := make(map[string]struct{})
	data := make(map[string]struct{})
	selected := false
	for _, service := range instance.childrenNamed("SERVICE") {
		serviceTemplate := resolver.byID[service.attr("tmplref")]
		if serviceTemplate == nil || serviceTemplate.name != "DCLSRVTMPL" || !directChild(classTemplate, serviceTemplate) {
			continue
		}
		protocolService := resolver.byID[serviceTemplate.attr("tmplref")]
		if protocolService == nil || protocolService.name != "PROTOCOLSERVICE" {
			continue
		}
		if sid, ok := requestServiceID(protocolService); ok && (sid == 0x22 || sid == 0x2e) {
			collectServiceComponents(protocolService, identifiers, data)
			selected = true
		}
	}
	return identifiers, data, selected
}

func directChild(parent, candidate *element) bool {
	for _, child := range parent.children {
		if child == candidate {
			return true
		}
	}
	return false
}

func collectServiceComponents(protocolService *element, identifiers, data map[string]struct{}) {
	for _, messageName := range []string{"REQ", "POS"} {
		message := protocolService.child(messageName)
		for _, component := range message.childrenNamed("STATICCOMP") {
			if component.attr("spec") == "id" && component.attr("id") != "" {
				identifiers[component.attr("id")] = struct{}{}
			}
		}
		for _, component := range message.childrenNamed("SIMPLEPROXYCOMP") {
			if component.attr("dest") == "data" && component.attr("id") != "" {
				data[component.attr("id")] = struct{}{}
			}
		}
	}
}

func requestServiceID(protocolService *element) (uint8, bool) {
	request := protocolService.child("REQ")
	for _, component := range request.childrenNamed("CONSTCOMP") {
		if component.attr("spec") != "sid" {
			continue
		}
		value, err := strconv.ParseUint(component.attr("v"), 10, 8)
		return uint8(value), err == nil
	}
	return 0, false
}

func (resolver *resolver) identifierStatic(classTemplate *element, allowed map[string]struct{}) (*element, error) {
	var identifier *element
	for _, static := range classTemplate.childrenNamed("SHSTATIC") {
		if static.attr("spec") != "id" {
			continue
		}
		if identifier != nil {
			return nil, sourceError(resolver.name, "DCLTMPL %q has multiple identifier statics", classTemplate.attr("id"))
		}
		identifier = static
	}
	if identifier == nil {
		return nil, sourceError(resolver.name, "DCLTMPL %q has no identifier static", classTemplate.attr("id"))
	}

	valid16BitReference := false
	for _, reference := range identifier.childrenNamed("STATICCOMPREF") {
		if _, ok := allowed[reference.attr("idref")]; !ok {
			continue
		}
		component := resolver.byID[reference.attr("idref")]
		if component == nil || component.name != "STATICCOMP" || component.attr("spec") != "id" {
			continue
		}
		datatype := resolver.datatypes[component.attr("dtref")]
		coded := datatype.child("CVALUETYPE")
		if coded != nil && coded.attr("bl") == "16" {
			valid16BitReference = true
			break
		}
	}
	if !valid16BitReference {
		return nil, sourceError(resolver.name, "DCLTMPL %q does not prove a 16-bit identifier", classTemplate.attr("id"))
	}
	return identifier, nil
}

// dataProxies returns the shared proxies carrying the data record of a data
// identifier class. Only newer CANdela versions mark them with
// spec="didDataReference", so the proxy is identified by the request and
// response components it references instead.
func (resolver *resolver) dataProxies(classTemplate *element, allowed map[string]struct{}) ([]*element, error) {
	var matched []*element
	for _, proxy := range classTemplate.childrenNamed("SHPROXY") {
		if proxy.attr("dest") != "data" || proxy.attr("id") == "" {
			continue
		}
		for _, reference := range proxy.childrenNamed("PROXYCOMPREF") {
			if _, ok := allowed[reference.attr("idref")]; !ok {
				continue
			}
			matched = append(matched, proxy)
			break
		}
	}
	if len(matched) == 0 {
		return nil, sourceError(resolver.name, "DCLTMPL %q has no data proxy", classTemplate.attr("id"))
	}
	return matched, nil
}

// resolveDataRecord resolves every data proxy enabled for one instance. Read
// and write services may use separate proxies, but one DID can represent them
// only when both describe the same record.
func (resolver *resolver) resolveDataRecord(instance *element, proxies []*element) ([]Field, uint32, uint32, error) {
	var fields []Field
	var bitLength, maxBitLength uint32
	for index, proxy := range proxies {
		var container *element
		for _, candidate := range instance.childrenNamed("SIMPLECOMPCONT") {
			if candidate.attr("shproxyref") == proxy.attr("id") {
				container = candidate
				break
			}
		}
		if container == nil {
			return nil, 0, 0, sourceError(resolver.name, "data proxy %q has no component container", proxy.attr("id"))
		}

		candidateFields, candidateLength, candidateMaxLength, err := resolver.resolveFields(container)
		if err != nil {
			return nil, 0, 0, err
		}
		if index == 0 {
			fields, bitLength, maxBitLength = candidateFields, candidateLength, candidateMaxLength
			continue
		}
		if candidateLength != bitLength || candidateMaxLength != maxBitLength || !reflect.DeepEqual(candidateFields, fields) {
			return nil, 0, 0, sourceError(resolver.name, "enabled services describe different data records")
		}
	}
	return fields, bitLength, maxBitLength, nil
}

// resolveFields returns the record layout with the bit lengths of its smallest
// and largest payload. The two differ only when the record ends in a
// variable-length field.
func (resolver *resolver) resolveFields(container *element) ([]Field, uint32, uint32, error) {
	var fields []Field
	var offset uint64
	if err := resolver.appendFields(container, &fields, &offset, make(map[string]struct{}), 0); err != nil {
		return nil, 0, 0, err
	}
	maximum := offset
	if len(fields) > 0 {
		last := fields[len(fields)-1]
		maximum += uint64(last.MaxBitSize()) - uint64(last.BitSize())
	}
	if maximum > math.MaxUint32 {
		return nil, 0, 0, sourceError(resolver.name, "data record exceeds the supported bit length")
	}
	return fields, uint32(offset), uint32(maximum), nil
}

// appendFields walks one record in document order. Element order is the only
// statement of layout a CDD record makes, so every item either contributes a
// field or advances the offset.
func (resolver *resolver) appendFields(
	record *element,
	fields *[]Field,
	offset *uint64,
	activeReferences map[string]struct{},
	depth int,
) error {
	if depth > maxRecordNesting {
		return sourceError(resolver.name, "data record exceeds the supported nesting depth of %d", maxRecordNesting)
	}
	for _, item := range record.children {
		// A variable-length field has no fixed end, so nothing can follow it.
		if length := len(*fields); length > 0 && (*fields)[length-1].Variable != nil {
			switch item.name {
			case "NAME", "QUAL", "DESC":
			default:
				return sourceError(resolver.name, "variable-length field %q is followed by %s", (*fields)[length-1].Name, item.name)
			}
		}
		switch item.name {
		case "NAME", "QUAL", "DESC":
			// Presentation, not layout.
		case "DATAOBJ":
			field, err := resolver.resolveField(item, uint32(*offset))
			if err != nil {
				return err
			}
			*fields = append(*fields, field)
			*offset += uint64(field.BitSize())
		case "GAPDATAOBJ":
			// Explicit padding between fields, named only by its width.
			gap, err := strconv.ParseUint(item.attr("bl"), 10, 32)
			if err != nil {
				return sourceError(resolver.name, "GAPDATAOBJ has invalid bit length %q", item.attr("bl"))
			}
			*offset += gap
		case "STRUCT":
			if err := resolver.appendFields(item, fields, offset, activeReferences, depth+1); err != nil {
				return err
			}
		case "DIDDATAREF":
			reference := item.attr("didRef")
			shared := resolver.byID[reference]
			if shared == nil || shared.name != "DID" {
				return sourceError(resolver.name, "DIDDATAREF %q does not resolve", reference)
			}
			if _, active := activeReferences[reference]; active {
				return sourceError(resolver.name, "DIDDATAREF %q forms a reference cycle", reference)
			}
			structure := shared.child("STRUCTURE")
			if structure == nil {
				return sourceError(resolver.name, "shared DID %q has no STRUCTURE", reference)
			}
			activeReferences[reference] = struct{}{}
			err := resolver.appendFields(structure, fields, offset, activeReferences, depth+1)
			delete(activeReferences, reference)
			if err != nil {
				return err
			}
		default:
			// UNION selects between alternative layouts, and MUX and the record
			// data types describe payloads whose shape depends on the response.
			// Neither is a fixed record.
			return sourceError(resolver.name, "data record contains unsupported %s", item.name)
		}
		if *offset > math.MaxUint32 {
			return sourceError(resolver.name, "data record exceeds the supported bit length")
		}
	}
	return nil
}

func (resolver *resolver) resolveField(data *element, offset uint32) (Field, error) {
	name := data.childText("QUAL")
	if name == "" {
		return Field{}, sourceError(resolver.name, "DATAOBJ has no QUAL")
	}
	datatype := resolver.datatypes[data.attr("dtref")]
	if datatype == nil {
		return Field{}, sourceError(resolver.name, "field %q references unknown datatype %q", name, data.attr("dtref"))
	}
	coded := datatype.child("CVALUETYPE")
	if coded == nil {
		return Field{}, sourceError(resolver.name, "datatype for field %q has no CVALUETYPE", name)
	}
	bitLength64, err := strconv.ParseUint(coded.attr("bl"), 10, 32)
	if err != nil || bitLength64 == 0 {
		return Field{}, sourceError(resolver.name, "field %q has invalid bit length %q", name, coded.attr("bl"))
	}
	bitLength := uint32(bitLength64)
	encoding := Encoding(coded.attr("enc"))
	switch encoding {
	case EncodingUnsigned, EncodingSigned, EncodingBCD, EncodingFloat, EncodingDouble, EncodingASCII, EncodingUTF:
	default:
		return Field{}, sourceError(resolver.name, "field %q uses unsupported encoding %q", name, encoding)
	}

	// A field quantity repeats the coded element: fixed-size text, serial
	// numbers, calibration blocks, and buffers whose length the ECU chooses. On
	// an atom the size bounds constrain the value rather than the repetition, so
	// they are not read here.
	count := uint32(1)
	var extent *Extent
	if coded.attr("qty") == "field" {
		minimum, minErr := strconv.ParseUint(coded.attr("minsz"), 10, 32)
		maximum, maxErr := strconv.ParseUint(coded.attr("maxsz"), 10, 32)
		switch {
		case minErr != nil || maxErr != nil || minimum > maximum:
			return Field{}, sourceError(resolver.name, "field %q has invalid size bounds %q to %q", name, coded.attr("minsz"), coded.attr("maxsz"))
		case maximum == 0:
			return Field{}, sourceError(resolver.name, "field %q is empty", name)
		case maximum > uint64(math.MaxUint32)/uint64(bitLength):
			return Field{}, sourceError(resolver.name, "field %q bit length overflows", name)
		}
		count = uint32(minimum)
		if minimum != maximum {
			extent = &Extent{MinCount: uint32(minimum), MaxCount: uint32(maximum)}
		}
	} else if quantity := coded.attr("qty"); quantity != "" && quantity != "atom" {
		return Field{}, sourceError(resolver.name, "field %q uses unsupported quantity %q", name, quantity)
	}

	var byteOrder ByteOrder
	switch coded.attr("bo") {
	case "21":
		byteOrder = ByteOrderBig
	case "12":
		byteOrder = ByteOrderLittle
	default:
		return Field{}, sourceError(resolver.name, "field %q uses unknown byte order %q", name, coded.attr("bo"))
	}

	field := Field{
		Name:      name,
		BitOffset: offset,
		BitLength: bitLength,
		Count:     count,
		Variable:  extent,
		ByteOrder: byteOrder,
		Encoding:  encoding,
	}
	if physical := datatype.child("PVALUETYPE"); physical != nil {
		field.Unit = physical.childText("UNIT")
	}
	if comp := datatype.child("COMP"); comp != nil {
		scale, scaleErr := strconv.ParseFloat(comp.attr("f"), 64)
		offset, offsetErr := strconv.ParseFloat(comp.attr("o"), 64)
		if scaleErr != nil || offsetErr != nil {
			return Field{}, sourceError(resolver.name, "field %q has invalid linear conversion", name)
		}
		field.Conversion = &LinearConversion{Scale: scale, Offset: offset}
	}
	for _, textMap := range datatype.childrenNamed("TEXTMAP") {
		first, firstErr := parseBound(textMap.attr("s"))
		last, lastErr := parseBound(textMap.attr("e"))
		if firstErr != nil || lastErr != nil {
			return Field{}, sourceError(resolver.name, "field %q has an invalid choice range %q to %q", name, textMap.attr("s"), textMap.attr("e"))
		}
		// Text maps also label whole bands, such as reserved or unused ranges.
		// A band is presentation rather than a distinct value, and dropping it
		// leaves the exact labels of the same field intact.
		if first != last {
			continue
		}
		label := textMap.child("TEXT").childText("TUV")
		if label == "" {
			return Field{}, sourceError(resolver.name, "field %q contains a choice with no label", name)
		}
		field.Choices = append(field.Choices, Choice{Value: first, Label: label})
	}
	return field, nil
}

func parseBound(value string) (int64, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "(")
	value = strings.TrimSuffix(value, ")")
	return strconv.ParseInt(value, 10, 64)
}

func sourceError(source, format string, args ...any) error {
	if source == "" {
		source = "CDD input"
	}
	return fmt.Errorf("%s: %s", source, fmt.Sprintf(format, args...))
}
