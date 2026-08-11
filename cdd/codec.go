package cdd

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"

	"github.com/tomrford/gocan/internal/scalar"
)

type recordCodec struct {
	fieldsByName map[string]struct{}
	err          error
}

// Encode encodes one complete DID data record. Every field requires a value.
// A linearly converted physical value is quantized to the nearest raw value,
// so Decode can differ from the encoded input by up to half a scale step.
func (record *Record) Encode(values Values) ([]byte, error) {
	codec, err := record.usableCodec()
	if err != nil {
		return nil, err
	}
	for name := range values {
		if _, ok := codec.fieldsByName[name]; !ok {
			return nil, fmt.Errorf("CDD DID %q has no field %q", record.Name, name)
		}
	}

	payload := make([]byte, record.Length)
	for _, field := range record.Fields {
		value, ok := values[field.Name]
		if !ok {
			return nil, fmt.Errorf("CDD DID %q requires field %q", record.Name, field.Name)
		}
		encoded, err := encodeField(field, value)
		if err != nil {
			return nil, fmt.Errorf("encode CDD field %q: %w", field.Name, err)
		}
		start := int(field.BitOffset / 8)
		end := start + len(encoded)
		if end > len(payload) {
			if field.Variable == nil || end > int(record.MaxLength) {
				return nil, fmt.Errorf("encode CDD field %q: encoded data exceeds DID length", field.Name)
			}
			payload = append(payload, make([]byte, end-len(payload))...)
		}
		copy(payload[start:end], encoded)
	}
	return payload, nil
}

// Decode decodes one complete DID data record into physical field values. A
// scalar value carrying a choice label decodes to its label; other values
// decode numerically.
func (record *Record) Decode(payload []byte) (Values, error) {
	if _, err := record.usableCodec(); err != nil {
		return nil, err
	}
	if err := record.validatePayloadLength(len(payload)); err != nil {
		return nil, err
	}
	values := make(Values, len(record.Fields))
	for _, field := range record.Fields {
		start := int(field.BitOffset / 8)
		end := start + int(field.BitSize()/8)
		if field.Variable != nil {
			end = len(payload)
		}
		value, err := decodeField(field, payload[start:end])
		if err != nil {
			return nil, fmt.Errorf("decode CDD field %q: %w", field.Name, err)
		}
		values[field.Name] = value
	}
	return values, nil
}

func (record *Record) usableCodec() (*recordCodec, error) {
	if record == nil {
		return nil, fmt.Errorf("CDD record is nil")
	}
	if record.codec == nil {
		return nil, fmt.Errorf("CDD DID %q record has no resolved codec; obtain records from Parse", record.Name)
	}
	if record.codec.err != nil {
		return nil, fmt.Errorf("CDD DID %q codec: %w", record.Name, record.codec.err)
	}
	return record.codec, nil
}

func compileRecordCodec(record *Record) *recordCodec {
	codec := &recordCodec{fieldsByName: make(map[string]struct{}, len(record.Fields))}
	for _, field := range record.Fields {
		if _, exists := codec.fieldsByName[field.Name]; exists {
			codec.err = fmt.Errorf("field name %q is repeated", field.Name)
			return codec
		}
		codec.fieldsByName[field.Name] = struct{}{}
		if field.BitOffset%8 != 0 || field.BitLength%8 != 0 {
			codec.err = fmt.Errorf("field %q is not byte-aligned", field.Name)
			return codec
		}
		if field.BitLength > 64 {
			codec.err = fmt.Errorf("field %q element width %d is outside 1 through 64 bits", field.Name, field.BitLength)
			return codec
		}
		if err := validateFieldEncoding(field); err != nil {
			codec.err = fmt.Errorf("field %q: %w", field.Name, err)
			return codec
		}
	}
	return codec
}

func validateFieldEncoding(field Field) error {
	switch field.Encoding {
	case EncodingUnsigned, EncodingSigned:
	case EncodingFloat:
		if field.BitLength != 32 {
			return fmt.Errorf("float requires 32 bits, got %d", field.BitLength)
		}
	case EncodingDouble:
		if field.BitLength != 64 {
			return fmt.Errorf("double requires 64 bits, got %d", field.BitLength)
		}
	case EncodingASCII:
		if field.BitLength != 8 {
			return fmt.Errorf("ASCII requires 8-bit elements, got %d", field.BitLength)
		}
	case EncodingBCD, EncodingUTF:
		return fmt.Errorf("encoding %q is not supported by the codec", field.Encoding)
	default:
		return fmt.Errorf("encoding %q is unknown", field.Encoding)
	}
	if field.Conversion != nil {
		if field.Encoding != EncodingUnsigned && field.Encoding != EncodingSigned {
			return fmt.Errorf("linear conversion requires an integer encoding")
		}
		if field.Conversion.Scale == 0 || math.IsNaN(field.Conversion.Scale) || math.IsInf(field.Conversion.Scale, 0) ||
			math.IsNaN(field.Conversion.Offset) || math.IsInf(field.Conversion.Offset, 0) {
			return fmt.Errorf("linear conversion must have a finite nonzero scale and finite offset")
		}
	}
	if len(field.Choices) > 0 && field.Encoding != EncodingUnsigned && field.Encoding != EncodingSigned {
		return fmt.Errorf("choices require an integer encoding")
	}
	return nil
}

func (record *Record) validatePayloadLength(length int) error {
	if length < int(record.Length) || length > int(record.MaxLength) {
		return fmt.Errorf("CDD DID %q payload length %d is outside %d through %d", record.Name, length, record.Length, record.MaxLength)
	}
	if record.Length == record.MaxLength {
		return nil
	}
	// resolveFields guarantees that the lengths differ only when the record
	// ends in a variable-length field, so last.Variable is non-nil here.
	last := record.Fields[len(record.Fields)-1]
	elementBytes := int(last.BitLength / 8)
	start := int(last.BitOffset / 8)
	if length < start || (length-start)%elementBytes != 0 {
		return fmt.Errorf("CDD DID %q payload length %d does not contain whole %d-byte elements for field %q", record.Name, length, elementBytes, last.Name)
	}
	count := (length - start) / elementBytes
	if count < int(last.Variable.MinCount) || count > int(last.Variable.MaxCount) {
		return fmt.Errorf("CDD DID %q field %q has %d elements, want %d through %d", record.Name, last.Name, count, last.Variable.MinCount, last.Variable.MaxCount)
	}
	return nil
}

func encodeField(field Field, value any) ([]byte, error) {
	if field.Encoding == EncodingASCII {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("ASCII value has type %T, want string", value)
		}
		encoded := []byte(text)
		for _, octet := range encoded {
			if octet > 0x7f {
				return nil, fmt.Errorf("ASCII value contains non-ASCII byte %#02x", octet)
			}
		}
		count := len(encoded)
		if err := validateElementCount(field, count); err != nil {
			return nil, err
		}
		return encoded, nil
	}

	count := int(field.Count)
	reflected := reflect.ValueOf(value)
	isArray := reflected.IsValid() && (reflected.Kind() == reflect.Slice || reflected.Kind() == reflect.Array)
	if field.Count != 1 || field.Variable != nil {
		if !isArray {
			return nil, fmt.Errorf("array value has type %T, want slice or array", value)
		}
		count = reflected.Len()
		if err := validateElementCount(field, count); err != nil {
			return nil, err
		}
	} else if isArray {
		return nil, fmt.Errorf("scalar value has type %T", value)
	}

	elementBytes := int(field.BitLength / 8)
	encoded := make([]byte, count*elementBytes)
	for index := range count {
		element := value
		if isArray {
			element = reflected.Index(index).Interface()
		}
		raw, err := encodeScalar(field, element, !isArray)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", index, err)
		}
		writeRaw(encoded[index*elementBytes:(index+1)*elementBytes], raw, field.ByteOrder)
	}
	return encoded, nil
}

func decodeField(field Field, encoded []byte) (any, error) {
	if field.Encoding == EncodingASCII {
		for _, octet := range encoded {
			if octet > 0x7f {
				return nil, fmt.Errorf("ASCII data contains non-ASCII byte %#02x", octet)
			}
		}
		return string(encoded), nil
	}
	elementBytes := int(field.BitLength / 8)
	count := len(encoded) / elementBytes
	if field.Count == 1 && field.Variable == nil {
		return decodeScalar(field, readRaw(encoded, field.ByteOrder), true), nil
	}
	switch fieldScalarKind(field) {
	case reflect.Uint64:
		values := make([]uint64, count)
		for index := range count {
			values[index] = decodeScalar(field, readRaw(encoded[index*elementBytes:(index+1)*elementBytes], field.ByteOrder), false).(uint64)
		}
		return values, nil
	case reflect.Int64:
		values := make([]int64, count)
		for index := range count {
			values[index] = decodeScalar(field, readRaw(encoded[index*elementBytes:(index+1)*elementBytes], field.ByteOrder), false).(int64)
		}
		return values, nil
	default:
		values := make([]float64, count)
		for index := range count {
			values[index] = decodeScalar(field, readRaw(encoded[index*elementBytes:(index+1)*elementBytes], field.ByteOrder), false).(float64)
		}
		return values, nil
	}
}

func validateElementCount(field Field, count int) error {
	minimum, maximum := int(field.Count), int(field.Count)
	if field.Variable != nil {
		minimum = int(field.Variable.MinCount)
		maximum = int(field.Variable.MaxCount)
	}
	if count < minimum || count > maximum {
		return fmt.Errorf("has %d elements, want %d through %d", count, minimum, maximum)
	}
	return nil
}

func encodeScalar(field Field, value any, allowLabel bool) (uint64, error) {
	if label, ok := value.(string); ok {
		if !allowLabel {
			return 0, fmt.Errorf("choice labels are supported only for scalar fields")
		}
		return scalar.RawForLabel(field.Choices, label, field.BitLength, field.Encoding == EncodingSigned)
	}
	if field.Encoding == EncodingFloat || field.Encoding == EncodingDouble {
		floating, err := scalar.NumericFloat(value)
		if err != nil {
			return 0, err
		}
		if math.IsNaN(floating) || math.IsInf(floating, 0) {
			return 0, fmt.Errorf("float value must be finite")
		}
		if field.Encoding == EncodingFloat {
			converted := float32(floating)
			if math.IsInf(float64(converted), 0) {
				return 0, fmt.Errorf("value exceeds float32")
			}
			return uint64(math.Float32bits(converted)), nil
		}
		return math.Float64bits(floating), nil
	}
	if field.Conversion != nil {
		if field.Conversion.Scale == 1 && field.Conversion.Offset == 0 {
			if field.Encoding == EncodingSigned {
				integer, err := scalar.ExactSigned(value)
				if err != nil {
					return 0, err
				}
				return scalar.EncodeSigned(field.BitLength, integer)
			}
			integer, err := scalar.ExactUnsigned(value)
			if err != nil {
				return 0, err
			}
			return scalar.EncodeUnsigned(field.BitLength, integer)
		}
		physical, err := scalar.NumericFloat(value)
		if err != nil {
			return 0, err
		}
		if math.IsNaN(physical) || math.IsInf(physical, 0) {
			return 0, fmt.Errorf("physical value must be a finite number")
		}
		return scalar.LinearRaw(field.BitLength, field.Encoding == EncodingSigned, physical, field.Conversion.Scale, field.Conversion.Offset)
	}
	if field.Encoding == EncodingSigned {
		integer, err := scalar.ExactSigned(value)
		if err != nil {
			return 0, err
		}
		return scalar.EncodeSigned(field.BitLength, integer)
	}
	integer, err := scalar.ExactUnsigned(value)
	if err != nil {
		return 0, err
	}
	return scalar.EncodeUnsigned(field.BitLength, integer)
}

// decodeScalar decodes one raw element. Labels mirror encodeScalar: only a
// scalar field decodes to its choice label, so array elements keep the numeric
// type their slice declares.
func decodeScalar(field Field, raw uint64, allowLabel bool) any {
	if field.Encoding == EncodingFloat {
		return float64(math.Float32frombits(uint32(raw)))
	}
	if field.Encoding == EncodingDouble {
		return math.Float64frombits(raw)
	}
	if field.Encoding == EncodingSigned {
		value := scalar.DecodeSigned(field.BitLength, raw)
		if allowLabel {
			if label, ok := scalar.Label(field.Choices, value); ok {
				return label
			}
		}
		if field.Conversion != nil {
			if field.Conversion.Scale == 1 && field.Conversion.Offset == 0 {
				return value
			}
			return float64(value)*field.Conversion.Scale + field.Conversion.Offset
		}
		return value
	}
	if allowLabel && raw <= math.MaxInt64 {
		if label, ok := scalar.Label(field.Choices, int64(raw)); ok {
			return label
		}
	}
	if field.Conversion != nil {
		if field.Conversion.Scale == 1 && field.Conversion.Offset == 0 {
			return raw
		}
		return float64(raw)*field.Conversion.Scale + field.Conversion.Offset
	}
	return raw
}

func fieldScalarKind(field Field) reflect.Kind {
	if field.Encoding == EncodingFloat || field.Encoding == EncodingDouble {
		return reflect.Float64
	}
	if field.Conversion != nil && (field.Conversion.Scale != 1 || field.Conversion.Offset != 0) {
		return reflect.Float64
	}
	if field.Encoding == EncodingSigned {
		return reflect.Int64
	}
	return reflect.Uint64
}

func writeRaw(destination []byte, raw uint64, order ByteOrder) {
	var bytes [8]byte
	if order == ByteOrderLittle {
		binary.LittleEndian.PutUint64(bytes[:], raw)
		copy(destination, bytes[:len(destination)])
		return
	}
	binary.BigEndian.PutUint64(bytes[:], raw)
	copy(destination, bytes[len(bytes)-len(destination):])
}

func readRaw(source []byte, order ByteOrder) uint64 {
	var bytes [8]byte
	if order == ByteOrderLittle {
		copy(bytes[:], source)
		return binary.LittleEndian.Uint64(bytes[:])
	}
	copy(bytes[len(bytes)-len(source):], source)
	return binary.BigEndian.Uint64(bytes[:])
}
