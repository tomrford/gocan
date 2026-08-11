// Package scalar converts physical Go values to and from raw coded scalars.
// It carries the numeric layer shared by the dbc and cdd codecs: exact integer
// conversion, sign handling within an arbitrary bit width, rounding linear
// conversions, and label lookup.
package scalar

import (
	"fmt"
	"math"
	"reflect"
)

// Choice assigns a label to one exact raw integer value.
type Choice struct {
	Value int64
	Label string
}

// Label returns the label of the first choice with value.
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

// EncodeSigned encodes value into the low bits as two's complement.
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

// EncodeUnsigned validates that value fits bits.
func EncodeUnsigned(bits uint32, value uint64) (uint64, error) {
	if bits < 64 && value >= uint64(1)<<bits {
		return 0, fmt.Errorf("unsigned value %d does not fit %d bits", value, bits)
	}
	return value, nil
}

// DecodeSigned sign-extends the low bits of raw.
func DecodeSigned(bits uint32, raw uint64) int64 {
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

// ExactSigned converts a Go numeric value to int64 without loss.
func ExactSigned(value any) (int64, error) {
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

// ExactUnsigned converts a Go numeric value to uint64 without loss.
func ExactUnsigned(value any) (uint64, error) {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return 0, fmt.Errorf("value is nil")
	}
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if reflected.Int() < 0 {
			return 0, fmt.Errorf("negative value %d for unsigned encoding", reflected.Int())
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

// NumericFloat converts a Go numeric value to float64. An integer that float64
// cannot represent exactly is rejected rather than rounded.
func NumericFloat(value any) (float64, error) {
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
