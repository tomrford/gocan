package cdd

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Parse parses the supported CDD subset into a resolved UDS DID catalog. It
// includes fixed-layout records used by ReadDataByIdentifier or
// WriteDataByIdentifier. Parse uses the first ECU and that ECU's first direct
// VAR. Other ECUs and variants are not inspected.
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
			if err != nil {
				return nil, err
			}
			if !selected {
				continue
			}
			if _, exists := database.didsByName[did.Name]; exists {
				return nil, sourceError(resolver.name, "duplicate DID name %q", did.Name)
			}
			if previous, exists := database.didsByIdentifier[did.Identifier]; exists {
				return nil, sourceError(resolver.name, "DIDs %q and %q use identifier %#04x", database.DIDs[previous].Name, did.Name, did.Identifier)
			}
			database.didsByName[did.Name] = len(database.DIDs)
			database.didsByIdentifier[did.Identifier] = len(database.DIDs)
			database.DIDs = append(database.DIDs, did)
		}
	}
	return database, nil
}

func (resolver *resolver) resolveDID(instance *element) (DID, bool, error) {
	classTemplate := resolver.byID[instance.attr("tmplref")]
	if classTemplate == nil || classTemplate.name != "DCLTMPL" {
		return DID{}, false, nil
	}

	identifierComponents := make(map[string]struct{})
	dataComponents := make(map[string]struct{})
	for _, service := range instance.childrenNamed("SERVICE") {
		serviceTemplate := resolver.byID[service.attr("tmplref")]
		if serviceTemplate == nil || serviceTemplate.name != "DCLSRVTMPL" || !directChild(classTemplate, serviceTemplate) {
			continue
		}
		protocolService := resolver.byID[serviceTemplate.attr("tmplref")]
		if protocolService == nil || protocolService.name != "PROTOCOLSERVICE" {
			continue
		}
		sid, ok := requestServiceID(protocolService)
		if ok && (sid == 0x22 || sid == 0x2e) {
			collectServiceComponents(protocolService, identifierComponents, dataComponents)
		}
	}
	if len(identifierComponents) == 0 {
		return DID{}, false, nil
	}

	identifierStatic, err := resolver.identifierStatic(classTemplate, identifierComponents)
	if err != nil {
		return DID{}, false, err
	}
	var identifierValue *element
	for _, value := range instance.childrenNamed("STATICVALUE") {
		if value.attr("shstaticref") == identifierStatic.attr("id") {
			identifierValue = value
			break
		}
	}
	if identifierValue == nil {
		return DID{}, false, sourceError(resolver.name, "selected diagnostic instance %q has no data identifier value", instance.childText("QUAL"))
	}
	identifier64, err := strconv.ParseUint(identifierValue.attr("v"), 0, 16)
	if err != nil {
		return DID{}, false, sourceError(resolver.name, "diagnostic instance %q has invalid data identifier %q", instance.childText("QUAL"), identifierValue.attr("v"))
	}

	if err := resolver.validateDataProxy(classTemplate, dataComponents); err != nil {
		return DID{}, false, err
	}
	container := instance.child("SIMPLECOMPCONT")
	if container == nil {
		return DID{}, false, sourceError(resolver.name, "selected DID %q has no SIMPLECOMPCONT", instance.childText("QUAL"))
	}
	fields, bitLength, err := resolver.resolveFields(container)
	if err != nil {
		return DID{}, false, fmt.Errorf("CDD DID %q: %w", instance.childText("QUAL"), err)
	}
	name := instance.childText("QUAL")
	if name == "" {
		return DID{}, false, sourceError(resolver.name, "selected DID %#04x has no QUAL", identifier64)
	}
	return DID{
		Name:       name,
		Identifier: uint16(identifier64),
		Length:     uint32((uint64(bitLength) + 7) / 8),
		Fields:     fields,
	}, true, nil
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
		value, err := strconv.ParseUint(component.attr("v"), 0, 8)
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

func (resolver *resolver) validateDataProxy(classTemplate *element, allowed map[string]struct{}) error {
	matched := 0
	for _, proxy := range classTemplate.childrenNamed("SHPROXY") {
		if proxy.attr("dest") != "data" || proxy.attr("spec") != "didDataReference" {
			continue
		}
		for _, reference := range proxy.childrenNamed("PROXYCOMPREF") {
			if _, ok := allowed[reference.attr("idref")]; ok {
				matched++
				break
			}
		}
	}
	if matched != 1 {
		return sourceError(resolver.name, "DCLTMPL %q has %d matching DID data proxies, want 1", classTemplate.attr("id"), matched)
	}
	return nil
}

func (resolver *resolver) resolveFields(container *element) ([]Field, uint32, error) {
	var fields []Field
	var offset uint64
	for _, item := range container.children {
		switch item.name {
		case "DATAOBJ":
			field, err := resolver.resolveField(item, uint32(offset))
			if err != nil {
				return nil, 0, err
			}
			fields = append(fields, field)
			offset += uint64(field.BitLength)
		case "DIDDATAREF":
			shared := resolver.byID[item.attr("didRef")]
			if shared == nil || shared.name != "DID" {
				return nil, 0, sourceError(resolver.name, "DIDDATAREF %q does not resolve", item.attr("didRef"))
			}
			structure := shared.child("STRUCTURE")
			if structure == nil {
				return nil, 0, sourceError(resolver.name, "shared DID %q has no STRUCTURE", item.attr("didRef"))
			}
			for _, data := range structure.children {
				if data.name != "DATAOBJ" {
					return nil, 0, sourceError(resolver.name, "shared DID %q contains unsupported %s", item.attr("didRef"), data.name)
				}
				field, err := resolver.resolveField(data, uint32(offset))
				if err != nil {
					return nil, 0, err
				}
				fields = append(fields, field)
				offset += uint64(field.BitLength)
			}
		default:
			return nil, 0, sourceError(resolver.name, "selected data record contains unsupported %s", item.name)
		}
		if offset > math.MaxUint32 {
			return nil, 0, sourceError(resolver.name, "selected data record exceeds the supported bit length")
		}
	}
	return fields, uint32(offset), nil
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
	case EncodingUnsigned, EncodingSigned, EncodingBCD, EncodingFloat:
		if coded.attr("qty") != "" && coded.attr("qty") != "atom" {
			return Field{}, sourceError(resolver.name, "field %q uses unsupported quantity %q", name, coded.attr("qty"))
		}
	case EncodingASCII, EncodingUTF:
		minimum, minErr := strconv.ParseUint(coded.attr("minsz"), 10, 32)
		maximum, maxErr := strconv.ParseUint(coded.attr("maxsz"), 10, 32)
		if coded.attr("qty") != "field" || minErr != nil || maxErr != nil || minimum == 0 || minimum != maximum {
			return Field{}, sourceError(resolver.name, "field %q is not fixed-size text", name)
		}
		if minimum > math.MaxUint32/uint64(bitLength) {
			return Field{}, sourceError(resolver.name, "field %q bit length overflows", name)
		}
		bitLength *= uint32(minimum)
	default:
		return Field{}, sourceError(resolver.name, "field %q uses unsupported encoding %q", name, encoding)
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
		if firstErr != nil || lastErr != nil || first != last {
			return Field{}, sourceError(resolver.name, "field %q contains an unsupported choice range", name)
		}
		label := textMap.firstText("TEXT", "TUV")
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
	return strconv.ParseInt(value, 0, 64)
}

func sourceError(source, format string, args ...any) error {
	if source == "" {
		source = "CDD input"
	}
	return fmt.Errorf("%s: %s", source, fmt.Sprintf(format, args...))
}
