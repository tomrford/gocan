package cdd

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
)

type recordCodec struct {
	fieldsByName map[string]struct{}
	err          error
}

// Encode encodes one complete DID data record. Every field requires a value.
func (record *Record) Encode(values Values) ([]byte, error) {
	codec, err := record.usableCodec()
	if err != nil {
		return nil, err
	}
	if len(values) != len(record.Fields) {
		for _, field := range record.Fields {
			if _, ok := values[field.Name]; !ok {
				return nil, fmt.Errorf("CDD DID %q requires field %q", record.name, field.Name)
			}
		}
		for name := range values {
			if _, ok := codec.fieldsByName[name]; !ok {
				return nil, fmt.Errorf("CDD DID %q has no field %q", record.name, name)
			}
		}
	}

	payload := make([]byte, record.Length)
	for _, field := range record.Fields {
		value, ok := values[field.Name]
		if !ok {
			return nil, fmt.Errorf("CDD DID %q requires field %q", record.name, field.Name)
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

// Decode decodes one complete DID data record into physical field values.
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
		return nil, fmt.Errorf("CDD DID %q record has no resolved codec; obtain records from Parse", record.name)
	}
	if record.codec.err != nil {
		return nil, fmt.Errorf("CDD DID %q codec: %w", record.name, record.codec.err)
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
		return fmt.Errorf("CDD DID %q payload length %d is outside %d through %d", record.name, length, record.Length, record.MaxLength)
	}
	if record.Length == record.MaxLength {
		if length != int(record.Length) {
			return fmt.Errorf("CDD DID %q payload length %d, want %d", record.name, length, record.Length)
		}
		return nil
	}
	last := record.Fields[len(record.Fields)-1]
	elementBytes := int(last.BitLength / 8)
	start := int(last.BitOffset / 8)
	if length < start || (length-start)%elementBytes != 0 {
		return fmt.Errorf("CDD DID %q payload length %d does not contain whole %d-byte elements for field %q", record.name, length, elementBytes, last.Name)
	}
	count := (length - start) / elementBytes
	if count < int(last.Variable.MinCount) || count > int(last.Variable.MaxCount) {
		return fmt.Errorf("CDD DID %q field %q has %d elements, want %d through %d", record.name, last.Name, count, last.Variable.MinCount, last.Variable.MaxCount)
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
		return decodeScalar(field, readRaw(encoded, field.ByteOrder)), nil
	}
	switch fieldScalarKind(field) {
	case reflect.Uint64:
		values := make([]uint64, count)
		for index := range count {
			values[index] = decodeScalar(field, readRaw(encoded[index*elementBytes:(index+1)*elementBytes], field.ByteOrder)).(uint64)
		}
		return values, nil
	case reflect.Int64:
		values := make([]int64, count)
		for index := range count {
			values[index] = decodeScalar(field, readRaw(encoded[index*elementBytes:(index+1)*elementBytes], field.ByteOrder)).(int64)
		}
		return values, nil
	default:
		values := make([]float64, count)
		for index := range count {
			values[index] = decodeScalar(field, readRaw(encoded[index*elementBytes:(index+1)*elementBytes], field.ByteOrder)).(float64)
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
		return rawChoice(field, label)
	}
	if field.Encoding == EncodingFloat || field.Encoding == EncodingDouble {
		floating, err := numericFloat(value)
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
				integer, err := exactSigned(value)
				if err != nil {
					return 0, err
				}
				return encodeSigned(field.BitLength, integer)
			}
			integer, err := exactUnsigned(value)
			if err != nil {
				return 0, err
			}
			return encodeUnsigned(field.BitLength, integer)
		}
		physical, err := numericFloat(value)
		if err != nil {
			return 0, err
		}
		if math.IsNaN(physical) || math.IsInf(physical, 0) {
			return 0, fmt.Errorf("physical value must be a finite number")
		}
		raw := (physical - field.Conversion.Offset) / field.Conversion.Scale
		rounded := math.Round(raw)
		reconstructed := rounded*field.Conversion.Scale + field.Conversion.Offset
		if math.IsNaN(raw) || math.IsInf(raw, 0) || !sameRepresentableFloat(physical, reconstructed) {
			return 0, fmt.Errorf("physical value %v does not map to an integer raw value", physical)
		}
		if math.Abs(rounded) > 1<<53 {
			return 0, fmt.Errorf("raw value %v cannot be represented exactly during linear conversion", rounded)
		}
		if field.Encoding == EncodingSigned {
			if rounded < -math.Exp2(63) || rounded >= math.Exp2(63) {
				return 0, fmt.Errorf("raw value %v exceeds int64", rounded)
			}
			return encodeSigned(field.BitLength, int64(rounded))
		}
		if rounded < 0 || rounded >= math.Exp2(64) {
			return 0, fmt.Errorf("raw value %v exceeds uint64", rounded)
		}
		return encodeUnsigned(field.BitLength, uint64(rounded))
	}
	if field.Encoding == EncodingSigned {
		integer, err := exactSigned(value)
		if err != nil {
			return 0, err
		}
		return encodeSigned(field.BitLength, integer)
	}
	integer, err := exactUnsigned(value)
	if err != nil {
		return 0, err
	}
	return encodeUnsigned(field.BitLength, integer)
}

func sameRepresentableFloat(left, right float64) bool {
	if left == right {
		return true
	}
	leftULP := math.Abs(math.Nextafter(left, math.Inf(1)) - left)
	rightULP := math.Abs(math.Nextafter(right, math.Inf(1)) - right)
	return math.Abs(left-right) <= max(leftULP, rightULP)
}

func decodeScalar(field Field, raw uint64) any {
	if field.Encoding == EncodingFloat {
		return float64(math.Float32frombits(uint32(raw)))
	}
	if field.Encoding == EncodingDouble {
		return math.Float64frombits(raw)
	}
	if field.Encoding == EncodingSigned {
		value := decodeSigned(field.BitLength, raw)
		if field.Conversion != nil {
			if field.Conversion.Scale == 1 && field.Conversion.Offset == 0 {
				return value
			}
			return float64(value)*field.Conversion.Scale + field.Conversion.Offset
		}
		return value
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

func rawChoice(field Field, label string) (uint64, error) {
	found := false
	var value int64
	for _, choice := range field.Choices {
		if choice.Label != label {
			continue
		}
		if found && value != choice.Value {
			return 0, fmt.Errorf("choice label %q is ambiguous", label)
		}
		found = true
		value = choice.Value
	}
	if !found {
		return 0, fmt.Errorf("unknown choice label %q", label)
	}
	if field.Encoding == EncodingSigned {
		return encodeSigned(field.BitLength, value)
	}
	if value < 0 {
		return 0, fmt.Errorf("negative choice %d for unsigned field", value)
	}
	return encodeUnsigned(field.BitLength, uint64(value))
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

func encodeSigned(bits uint32, value int64) (uint64, error) {
	if bits == 64 {
		return uint64(value), nil
	}
	minimum := -(int64(1) << (bits - 1))
	maximum := (int64(1) << (bits - 1)) - 1
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("signed value %d does not fit %d bits", value, bits)
	}
	return uint64(value) & (uint64(1)<<bits - 1), nil
}

func encodeUnsigned(bits uint32, value uint64) (uint64, error) {
	if bits < 64 && value >= uint64(1)<<bits {
		return 0, fmt.Errorf("unsigned value %d does not fit %d bits", value, bits)
	}
	return value, nil
}

func decodeSigned(bits uint32, raw uint64) int64 {
	if bits == 64 {
		return int64(raw)
	}
	mask := uint64(1)<<bits - 1
	raw &= mask
	if raw&(uint64(1)<<(bits-1)) != 0 {
		raw |= ^mask
	}
	return int64(raw)
}

func exactSigned(value any) (int64, error) {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return 0, fmt.Errorf("value is nil")
	}
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflected.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if reflected.Uint() > math.MaxInt64 {
			return 0, fmt.Errorf("unsigned value %d exceeds int64", reflected.Uint())
		}
		return int64(reflected.Uint()), nil
	case reflect.Float32, reflect.Float64:
		floating := reflected.Float()
		if math.IsNaN(floating) || math.IsInf(floating, 0) || floating != math.Trunc(floating) || floating < -math.Exp2(63) || floating >= math.Exp2(63) {
			return 0, fmt.Errorf("value %v is not an exact int64", floating)
		}
		return int64(floating), nil
	default:
		return 0, fmt.Errorf("value has unsupported type %T", value)
	}
}

func exactUnsigned(value any) (uint64, error) {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return 0, fmt.Errorf("value is nil")
	}
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if reflected.Int() < 0 {
			return 0, fmt.Errorf("negative value %d for unsigned field", reflected.Int())
		}
		return uint64(reflected.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return reflected.Uint(), nil
	case reflect.Float32, reflect.Float64:
		floating := reflected.Float()
		if math.IsNaN(floating) || math.IsInf(floating, 0) || floating != math.Trunc(floating) || floating < 0 || floating >= math.Exp2(64) {
			return 0, fmt.Errorf("value %v is not an exact uint64", floating)
		}
		return uint64(floating), nil
	default:
		return 0, fmt.Errorf("value has unsupported type %T", value)
	}
}

func numericFloat(value any) (float64, error) {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return 0, fmt.Errorf("value is nil")
	}
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		integer := reflected.Int()
		floating := float64(integer)
		if floating >= math.Exp2(63) || int64(floating) != integer {
			return 0, fmt.Errorf("integer value %d cannot be represented exactly as float64", integer)
		}
		return floating, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		integer := reflected.Uint()
		floating := float64(integer)
		if floating >= math.Exp2(64) || uint64(floating) != integer {
			return 0, fmt.Errorf("integer value %d cannot be represented exactly as float64", integer)
		}
		return floating, nil
	case reflect.Float32, reflect.Float64:
		return reflected.Float(), nil
	default:
		return 0, fmt.Errorf("value has type %T, want a number", value)
	}
}
