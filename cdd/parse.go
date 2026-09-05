package cdd

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const maxRecordNesting = 256

func (resolver *resolver) resolveRecord(name string, instance, classTemplate *element, componentID string) (*Record, error) {
	dataProxy, err := resolver.dataProxy(classTemplate, componentID)
	if err != nil {
		return nil, err
	}
	if _, err := resolver.reference(dataProxy.attr("id"), "SHPROXY"); err != nil {
		return nil, err
	}
	var selected *element
	for _, container := range instance.childrenNamed("SIMPLECOMPCONT") {
		if container.attr("shproxyref") != dataProxy.attr("id") {
			continue
		}
		if selected != nil {
			return nil, sourceError(resolver.name, "multiple component containers for data proxy %q", dataProxy.attr("id"))
		}
		selected = container
	}
	if selected == nil {
		return nil, sourceError(resolver.name, "instance has no component container for data proxy %q", dataProxy.attr("id"))
	}
	fields, bitLength, maxBitLength, err := resolver.resolveFields(selected)
	if err != nil {
		return nil, err
	}
	record := &Record{
		Metadata: metadata(selected), Name: name,
		Length:    uint32((uint64(bitLength) + 7) / 8),
		MaxLength: uint32((uint64(maxBitLength) + 7) / 8), Fields: fields,
	}
	record.codec = compileRecordCodec(record)
	return record, nil
}

// dataProxy returns the proxy carrying one operation's data record. Only newer
// CANdela versions mark it with
// spec="didDataReference", so the proxy is identified by the request and
// response components it references instead.
func (resolver *resolver) dataProxy(classTemplate *element, componentID string) (*element, error) {
	var matched *element
	for _, proxy := range classTemplate.childrenNamed("SHPROXY") {
		if proxy.attr("dest") != "data" || proxy.attr("id") == "" {
			continue
		}
		for _, reference := range proxy.childrenNamed("PROXYCOMPREF") {
			if reference.attr("idref") != componentID {
				continue
			}
			if matched != nil {
				return nil, sourceError(resolver.name, "DCLTMPL %q has more than one data proxy", classTemplate.attr("id"))
			}
			matched = proxy
			break
		}
	}
	if matched == nil {
		return nil, sourceError(resolver.name, "DCLTMPL %q has no data proxy", classTemplate.attr("id"))
	}
	return matched, nil
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
			start := len(*fields)
			if err := resolver.appendFields(item, fields, offset, activeReferences, depth+1); err != nil {
				return err
			}
			prependGroup((*fields)[start:], metadata(item))
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
			start := len(*fields)
			activeReferences[reference] = struct{}{}
			err := resolver.appendFields(structure, fields, offset, activeReferences, depth+1)
			delete(activeReferences, reference)
			if err != nil {
				return err
			}
			prependGroup((*fields)[start:], metadata(shared))
			prependGroup((*fields)[start:], metadata(item))
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
		Metadata:  metadata(data),
		Datatype:  metadata(datatype),
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

func prependGroup(fields []Field, group Metadata) {
	for i := range fields {
		fields[i].Groups = append([]Metadata{group}, fields[i].Groups...)
	}
}
