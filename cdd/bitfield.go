package cdd

import (
	"fmt"
	"math"
	"strconv"
)

// CANdelaStudio 17 English help (installed documentation transcribed in the
// task) defines STRUCT children from the enclosing value's least-significant
// bit. Byte order applies to that entire value, not individual children.
// Top-level objects remain byte-aligned. Explicit and implicit gaps reserve
// space without constraining received bits; request padding starts at zero.
func (resolver *resolver) appendBitfield(node *element, fields *[]Field, offset *uint64) error {
	container, err := resolver.resolveFieldDatatype(node, "STRUCT", uint32(*offset))
	if err != nil {
		return err
	}
	datatype := resolver.datatypes[node.attr("dtref")]
	if (datatype.name != "IDENT" && datatype.name != "TEXTTBL") || container.Conversion != nil || container.Variable != nil || container.BitSize()%8 != 0 || *offset%8 != 0 {
		return sourceError(resolver.name, "STRUCT requires a fixed, byte-aligned IDENT or TEXTTBL container")
	}
	if *offset+uint64(container.BitSize()) > math.MaxUint32 {
		return sourceError(resolver.name, "STRUCT exceeds the supported record bit length")
	}
	order := container.ByteOrder
	if datatype.child("CVALUETYPE").attr("qty") == "field" {
		if datatype.name != "IDENT" {
			return sourceError(resolver.name, "STRUCT array containers require IDENT")
		}
		if container.BitLength != 8 || (container.Encoding != EncodingUnsigned && container.Encoding != EncodingSigned && container.Encoding != EncodingASCII) {
			return sourceError(resolver.name, "STRUCT arrays other than one-byte integer/ASCII elements are unsupported")
		}
		reverse, err := resolver.reverseBitfieldBytes(datatype)
		if err != nil {
			return sourceError(resolver.name, "STRUCT: %v", err)
		}
		order = ByteOrderBig
		if reverse {
			order = ByteOrderLittle
		}
	} else if container.BitLength > 64 || (container.Encoding != EncodingUnsigned && container.Encoding != EncodingSigned) {
		return sourceError(resolver.name, "STRUCT atomic containers require integers up to 64 bits")
	}
	// Only the enclosing coded representation controls packing; choice labels
	// belong to each child's datatype, not the container's text table.
	bitfield := &Bitfield{Datatype: container.Datatype, BitOffset: uint32(*offset), BitLength: container.BitSize(), ByteOrder: order}
	var position uint64
	start := len(*fields)
	for _, child := range node.children {
		switch child.name {
		case "NAME", "QUAL", "DESC":
		case "DATAOBJ":
			field, err := resolver.resolveField(child, uint32(position))
			if err != nil {
				return err
			}
			if field.Variable != nil || field.Count != 1 || resolver.datatypes[child.attr("dtref")].child("CVALUETYPE").attr("qty") == "field" ||
				(field.Encoding != EncodingUnsigned && field.Encoding != EncodingSigned) || field.BitLength > 64 {
				return sourceError(resolver.name, "STRUCT child %q requires an atomic integer up to 64 bits", field.Name)
			}
			field.Bitfield = bitfield
			*fields = append(*fields, field)
			position += uint64(field.BitLength)
		case "GAPDATAOBJ":
			gap, err := strconv.ParseUint(child.attr("bl"), 10, 32)
			if err != nil {
				return sourceError(resolver.name, "STRUCT gap has invalid bit length %q", child.attr("bl"))
			}
			position += gap
		case "STRUCT":
			return sourceError(resolver.name, "nested STRUCT bitfields are prohibited")
		default:
			return sourceError(resolver.name, "STRUCT contains unsupported %s", child.name)
		}
		if position > uint64(bitfield.BitLength) {
			return sourceError(resolver.name, "STRUCT children exceed the enclosing datatype size")
		}
	}
	prependGroup((*fields)[start:], metadata(node))
	*offset += uint64(bitfield.BitLength)
	return nil
}

// Attribute IDs are document-local. Within DEFATTS/DATATYPEATTS, ENUMDEF.QUAL
// identifies the behaviour; ENUM.attrref selects that declaration and ENUM.v
// overrides ENUMDEF.v.
func (resolver *resolver) reverseBitfieldBytes(datatype *element) (bool, error) {
	var declaration *element
	var candidates []*element
	if definitions := resolver.ecuDoc.child("DEFATTS"); definitions != nil {
		for _, scope := range definitions.childrenNamed("DATATYPEATTS") {
			candidates = append(candidates, scope.children...)
		}
	}
	for _, candidate := range candidates {
		switch candidate.childText("QUAL") {
		case "ReverseBitfieldBytes", "ReverseBitFieldBytes":
		default:
			continue
		}
		if declaration != nil || candidate.name != "ENUMDEF" {
			return false, fmt.Errorf("ReverseBitFieldBytes declaration is ambiguous or not an enumeration")
		}
		var err error
		declaration, err = resolver.reference(candidate.attr("id"), "ENUMDEF")
		if err != nil {
			return false, err
		}
	}
	var override *element
	for _, attribute := range datatype.children {
		if attribute.name != "ENUM" {
			if declaration != nil && attribute.attr("attrref") == declaration.attr("id") {
				return false, fmt.Errorf("ReverseBitFieldBytes requires an ENUM value")
			}
			continue
		}
		target, err := resolver.reference(attribute.attr("attrref"), "ENUMDEF")
		if err != nil {
			return false, err
		}
		if target != declaration {
			continue
		}
		if override != nil {
			return false, fmt.Errorf("ReverseBitFieldBytes is repeated")
		}
		override = attribute
	}
	if declaration == nil {
		return false, nil
	}
	value := declaration.attr("v")
	if override != nil {
		value = override.attr("v")
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return false, fmt.Errorf("invalid ReverseBitFieldBytes value %q", value)
	}
	return number != 0, nil
}

func packedBit(field Field, bit uint32) (int, byte) {
	position := field.BitOffset + bit
	index := position / 8
	if field.Bitfield.ByteOrder == ByteOrderBig {
		index = field.Bitfield.BitLength/8 - 1 - index
	}
	return int(field.Bitfield.BitOffset/8 + index), byte(1 << (position % 8))
}

func readPacked(payload []byte, field Field) uint64 {
	var raw uint64
	for bit := uint32(0); bit < field.BitLength; bit++ {
		index, mask := packedBit(field, bit)
		if payload[index]&mask != 0 {
			raw |= uint64(1) << bit
		}
	}
	return raw
}

func writePacked(payload []byte, field Field, raw uint64) {
	for bit := uint32(0); bit < field.BitLength; bit++ {
		index, mask := packedBit(field, bit)
		payload[index] &^= mask
		if raw&(uint64(1)<<bit) != 0 {
			payload[index] |= mask
		}
	}
}
