package scalar

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
)

type Choice struct {
	Value int64
	Label string
}

// StringValue matches values of string kind, so a named string type carries a
// label or text the same way a plain string does, except json.Number, which
// always denotes a number.
func StringValue(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case json.Number:
		return "", false
	}
	reflected := reflect.ValueOf(value)
	if reflected.IsValid() && reflected.Kind() == reflect.String {
		return reflected.String(), true
	}
	return "", false
}

// DecodeLinear decodes one raw integer element to its physical value under
// physical = raw*scale + offset, or to its label when allowLabel is set and
// the raw value carries one. A scale of 1 with offset 0 preserves the integer
// type instead of producing a float64.
func DecodeLinear(bits uint32, signed bool, raw uint64, scale, offset float64, choices []Choice, allowLabel bool) any {
	if signed {
		value := DecodeSigned(bits, raw)
		if allowLabel {
			if label, ok := Label(choices, value); ok {
				return label
			}
		}
		if scale == 1 && offset == 0 {
			return value
		}
		return float64(value)*scale + offset
	}
	if allowLabel && raw <= math.MaxInt64 {
		if label, ok := Label(choices, int64(raw)); ok {
			return label
		}
	}
	if scale == 1 && offset == 0 {
		return raw
	}
	return float64(raw)*scale + offset
}

func Label(choices []Choice, value int64) (string, bool) {
	for _, choice := range choices {
		if choice.Value == value {
			return choice.Label, true
		}
	}
	return "", false
}

// RawForLabel returns the raw encoding of the choice labelled label. A label
// that appears with two different values is ambiguous.
func RawForLabel(choices []Choice, label string, bits uint32, signed bool) (uint64, error) {
	found := false
	var value int64
	for _, choice := range choices {
		if choice.Label != label {
			continue
		}
		if found && value != choice.Value {
			return 0, fmt.Errorf("label %q is ambiguous", label)
		}
		found = true
		value = choice.Value
	}
	if !found {
		return 0, fmt.Errorf("unknown label %q", label)
	}
	if signed {
		return EncodeSigned(bits, value)
	}
	if value < 0 {
		return 0, fmt.Errorf("negative value %d for unsigned encoding", value)
	}
	return EncodeUnsigned(bits, uint64(value))
}

// LinearRaw converts a finite physical value to the nearest raw integer under
// physical = raw*scale + offset, then encodes it into bits. Physical values
// between raw grid points are rounded, not rejected; a raw magnitude beyond
// 2^53 is rejected because float64 cannot produce it exactly.
func LinearRaw(bits uint32, signed bool, physical, scale, offset float64) (uint64, error) {
	raw := (physical - offset) / scale
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		return 0, fmt.Errorf("physical value %v does not produce a finite raw value", physical)
	}
	rounded := math.Round(raw)
	if math.Abs(rounded) > 1<<53 {
		return 0, fmt.Errorf("raw value %v cannot be represented exactly during linear conversion", rounded)
	}
	if signed {
		return EncodeSigned(bits, int64(rounded))
	}
	if rounded < 0 {
		return 0, fmt.Errorf("negative raw value %v for unsigned encoding", rounded)
	}
	return EncodeUnsigned(bits, uint64(rounded))
}

func EncodeSigned(bits uint32, value int64) (uint64, error) {
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

func EncodeUnsigned(bits uint32, value uint64) (uint64, error) {
	if bits < 64 && value >= uint64(1)<<bits {
		return 0, fmt.Errorf("unsigned value %d does not fit %d bits", value, bits)
	}
	return value, nil
}

// DecodeSigned reads the low bits (1 through 64) of raw as a two's-complement
// integer.
func DecodeSigned(bits uint32, raw uint64) int64 {
	shift := 64 - bits
	return int64(raw<<shift) >> shift
}

type numberKind uint8

const (
	notNumber numberKind = iota
	signedNumber
	unsignedNumber
	floatNumber
)

// goNumber is a Go integer or floating-point value, widened to 64 bits.
type goNumber struct {
	kind     numberKind
	signed   int64
	unsigned uint64
	float    float64
}

// numberOf classifies value by kind, so named numeric types convert like
// their underlying type. Builtin types avoid reflection.
func numberOf(value any) goNumber {
	switch value := value.(type) {
	case int:
		return goNumber{kind: signedNumber, signed: int64(value)}
	case int64:
		return goNumber{kind: signedNumber, signed: value}
	case int32:
		return goNumber{kind: signedNumber, signed: int64(value)}
	case int16:
		return goNumber{kind: signedNumber, signed: int64(value)}
	case int8:
		return goNumber{kind: signedNumber, signed: int64(value)}
	case uint:
		return goNumber{kind: unsignedNumber, unsigned: uint64(value)}
	case uint64:
		return goNumber{kind: unsignedNumber, unsigned: value}
	case uint32:
		return goNumber{kind: unsignedNumber, unsigned: uint64(value)}
	case uint16:
		return goNumber{kind: unsignedNumber, unsigned: uint64(value)}
	case uint8:
		return goNumber{kind: unsignedNumber, unsigned: uint64(value)}
	case float64:
		return goNumber{kind: floatNumber, float: value}
	case float32:
		return goNumber{kind: floatNumber, float: float64(value)}
	case nil, string, bool, json.Number:
		return goNumber{}
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return goNumber{kind: signedNumber, signed: reflected.Int()}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return goNumber{kind: unsignedNumber, unsigned: reflected.Uint()}
	case reflect.Float32, reflect.Float64:
		return goNumber{kind: floatNumber, float: reflected.Float()}
	default:
		return goNumber{}
	}
}

func ExactSigned(value any) (int64, error) {
	if number, ok := value.(json.Number); ok {
		if !strings.ContainsAny(string(number), ".eE") && validJSONNumber(string(number)) {
			if integer, err := number.Int64(); err == nil {
				return integer, nil
			}
		}
		rational, err := jsonRational(number)
		if err != nil {
			return 0, err
		}
		if !rational.IsInt() || !rational.Num().IsInt64() {
			return 0, fmt.Errorf("value %s is not an exact int64", number)
		}
		return rational.Num().Int64(), nil
	}
	switch number := numberOf(value); number.kind {
	case signedNumber:
		return number.signed, nil
	case unsignedNumber:
		if number.unsigned > math.MaxInt64 {
			return 0, fmt.Errorf("unsigned value %d exceeds int64", number.unsigned)
		}
		return int64(number.unsigned), nil
	case floatNumber:
		floating := number.float
		if math.IsNaN(floating) || math.IsInf(floating, 0) || floating != math.Trunc(floating) || floating < -math.Exp2(63) || floating >= math.Exp2(63) {
			return 0, fmt.Errorf("value %v is not an exact int64", floating)
		}
		return int64(floating), nil
	default:
		return 0, unsupportedType(value)
	}
}

func ExactUnsigned(value any) (uint64, error) {
	if number, ok := value.(json.Number); ok {
		if !strings.ContainsAny(string(number), ".eE") && validJSONNumber(string(number)) {
			if integer, err := strconv.ParseUint(string(number), 10, 64); err == nil {
				return integer, nil
			}
		}
		rational, err := jsonRational(number)
		if err != nil {
			return 0, err
		}
		if !rational.IsInt() || !rational.Num().IsUint64() {
			return 0, fmt.Errorf("value %s is not an exact uint64", number)
		}
		return rational.Num().Uint64(), nil
	}
	switch number := numberOf(value); number.kind {
	case signedNumber:
		if number.signed < 0 {
			return 0, fmt.Errorf("negative value %d for unsigned encoding", number.signed)
		}
		return uint64(number.signed), nil
	case unsignedNumber:
		return number.unsigned, nil
	case floatNumber:
		floating := number.float
		if math.IsNaN(floating) || math.IsInf(floating, 0) || floating != math.Trunc(floating) || floating < 0 || floating >= math.Exp2(64) {
			return 0, fmt.Errorf("value %v is not an exact uint64", floating)
		}
		return uint64(floating), nil
	default:
		return 0, unsupportedType(value)
	}
}

func unsupportedType(value any) error {
	if value == nil {
		return fmt.Errorf("value is nil")
	}
	return fmt.Errorf("value has unsupported type %T", value)
}

// NumericFloat rounds a Go numeric value or json.Number directly to bits
// (32 or 64) of floating-point precision, returning the result as float64.
// JSON numbers may underflow to zero but must not overflow.
func NumericFloat(value any, bits int) (float64, error) {
	if number, ok := value.(json.Number); ok {
		if !validJSONNumber(string(number)) {
			return 0, fmt.Errorf("invalid JSON number %q", number)
		}
		return strconv.ParseFloat(string(number), bits)
	}
	switch number := numberOf(value); number.kind {
	case signedNumber:
		if bits == 32 {
			return float64(float32(number.signed)), nil
		}
		return float64(number.signed), nil
	case unsignedNumber:
		if bits == 32 {
			return float64(float32(number.unsigned)), nil
		}
		return float64(number.unsigned), nil
	case floatNumber:
		if bits == 32 {
			return float64(float32(number.float)), nil
		}
		return number.float, nil
	default:
		if value == nil {
			return 0, fmt.Errorf("value is nil")
		}
		return 0, fmt.Errorf("value has type %T, want a number", value)
	}
}

// LinearFloat converts a physical value for linear integer encoding. Native
// integers must survive the float64 conversion exactly: rounding before an
// offset or scale is applied can change the raw integer. JSON numbers and
// floating-point inputs retain their normal float64 rounding behaviour.
func LinearFloat(value any) (float64, error) {
	switch number := numberOf(value); number.kind {
	case signedNumber:
		integer := number.signed
		floating := float64(integer)
		if floating >= math.Exp2(63) || int64(floating) != integer {
			return 0, fmt.Errorf("integer value %d cannot be represented exactly as float64", integer)
		}
		return floating, nil
	case unsignedNumber:
		integer := number.unsigned
		floating := float64(integer)
		if floating >= math.Exp2(64) || uint64(floating) != integer {
			return 0, fmt.Errorf("integer value %d cannot be represented exactly as float64", integer)
		}
		return floating, nil
	default:
		return NumericFloat(value, 64)
	}
}

// validJSONNumber reports whether s is exactly one JSON number. Unlike
// json.Valid it needs no []byte and rejects surrounding whitespace and other
// JSON values, which strconv would reject later anyway.
func validJSONNumber(s string) bool {
	s = strings.TrimPrefix(s, "-")
	switch {
	case s == "":
		return false
	case s[0] == '0':
		s = s[1:]
	case '1' <= s[0] && s[0] <= '9':
		s = skipDigits(s[1:])
	default:
		return false
	}
	if s != "" && s[0] == '.' {
		if len(s) < 2 || !isDigit(s[1]) {
			return false
		}
		s = skipDigits(s[2:])
	}
	if s != "" && (s[0] == 'e' || s[0] == 'E') {
		s = s[1:]
		if s != "" && (s[0] == '+' || s[0] == '-') {
			s = s[1:]
		}
		if s == "" || !isDigit(s[0]) {
			return false
		}
		s = skipDigits(s[1:])
	}
	return s == ""
}

func skipDigits(s string) string {
	for s != "" && isDigit(s[0]) {
		s = s[1:]
	}
	return s
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// jsonRational is the fallback for exact integer conversion. A float magnitude
// check rejects huge exponents before Rat.SetString can expand them. Rounded
// boundary values still reach the exact check; float64 never supplies the integer.
func jsonRational(number json.Number) (*big.Rat, error) {
	floating, err := NumericFloat(number, 64)
	if err != nil {
		return nil, err
	}
	if floating == 0 {
		mantissa, _, _ := strings.Cut(strings.ToLower(string(number)), "e")
		if strings.Trim(mantissa, "-0.") == "" {
			return new(big.Rat), nil
		}
	}
	if math.Abs(floating) < 1 || floating < -0x1p63 || floating > 0x1p64 {
		return nil, fmt.Errorf("value %s is outside the integer range", number)
	}
	if rational, ok := new(big.Rat).SetString(string(number)); ok {
		return rational, nil
	}
	return nil, fmt.Errorf("invalid or unsupported JSON number %q", number)
}
