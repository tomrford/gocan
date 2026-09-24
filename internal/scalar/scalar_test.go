package scalar

import (
	"encoding/json"
	"math"
	"testing"
)

// TestBoundaries pins the edges the dbc and cdd fixtures cannot reach: 64-bit
// sign handling, the 2^53 linear-conversion guard, and integer exactness.
func TestBoundaries(t *testing.T) {
	if raw, err := EncodeSigned(64, math.MinInt64); err != nil || DecodeSigned(64, raw) != math.MinInt64 {
		t.Fatalf("64-bit MinInt64 round trip = %#x, %v", raw, err)
	}
	if DecodeSigned(1, 1) != -1 {
		t.Fatal("1-bit raw 1 did not sign-extend to -1")
	}
	if _, err := EncodeSigned(8, 128); err == nil {
		t.Fatal("EncodeSigned fit 128 into 8 bits")
	}
	if _, err := LinearRaw(64, false, 1<<53, 1, 0); err != nil {
		t.Fatalf("LinearRaw rejected a raw value of exactly 2^53: %v", err)
	}
	if _, err := LinearRaw(64, false, 1<<53+2, 1, 0); err == nil {
		t.Fatal("LinearRaw accepted a raw value beyond 2^53")
	}
	if raw, err := LinearRaw(8, false, -0.4, 1, 0); err != nil || raw != 0 {
		t.Fatalf("LinearRaw(-0.4) = %#x, %v, want raw 0", raw, err)
	}
	if _, err := NumericFloat(uint64(1<<53 + 1)); err == nil {
		t.Fatal("NumericFloat accepted an integer float64 cannot represent")
	}
	if value, err := NumericFloat(int64(math.MinInt64)); err != nil || value != -math.Exp2(63) {
		t.Fatalf("NumericFloat(MinInt64) = %v, %v", value, err)
	}
}

func TestJSONIntegers(t *testing.T) {
	for _, test := range []struct {
		number               json.Number
		signed               int64
		unsigned             uint64
		signedOK, unsignedOK bool
	}{
		{"0", 0, 0, true, true},
		{"-0.0", 0, 0, true, true},
		{"1.0", 1, 1, true, true},
		{"1e3", 1000, 1000, true, true},
		{"1200e-2", 12, 12, true, true},
		{"-2E+1", -20, 0, true, false},
		{"9007199254740993.0", 9007199254740993, 9007199254740993, true, true},
		{"9223372036854775807", math.MaxInt64, math.MaxInt64, true, true},
		{"-9223372036854775808", math.MinInt64, 0, true, false},
		{"9223372036854775808", 0, 1 << 63, false, true},
		{"18446744073709551615", 0, math.MaxUint64, false, true},
		{"1.8446744073709551615e19", 0, math.MaxUint64, false, true},
		{"18446744073709551616", 0, 0, false, false},
		{"-9223372036854775809", 0, 0, false, false},
		{"1.00000000000000000001", 0, 0, false, false},
		{"1e-400", 0, 0, false, false},
	} {
		t.Run(string(test.number), func(t *testing.T) {
			if value, err := ExactSigned(test.number); (err == nil) != test.signedOK || err == nil && value != test.signed {
				t.Fatalf("ExactSigned = %d, %v; want %d, success %t", value, err, test.signed, test.signedOK)
			}
			if value, err := ExactUnsigned(test.number); (err == nil) != test.unsignedOK || err == nil && value != test.unsigned {
				t.Fatalf("ExactUnsigned = %d, %v; want %d, success %t", value, err, test.unsigned, test.unsignedOK)
			}
		})
	}
}

func TestJSONFloats(t *testing.T) {
	for _, test := range []struct {
		number json.Number
		want   float64
		ok     bool
	}{
		{"-0.0", math.Copysign(0, -1), true},
		{"0.1", 0.1, true},
		{"2.5e-1", 0.25, true},
		{"9007199254740992", 1 << 53, true},
		{"9007199254740993", 0, false},
		{"9007199254740993.0", 0, false},
		{"9.007199254740993e15", 0, false},
		{"9007199254740994", 1<<53 + 2, true},
		{"-9223372036854775808", -0x1p63, true},
		{"18446744073709551615", 0, false},
		{"1e400", 0, false},
	} {
		t.Run(string(test.number), func(t *testing.T) {
			if value, err := NumericFloat(test.number); (err == nil) != test.ok || err == nil && math.Float64bits(value) != math.Float64bits(test.want) {
				t.Fatalf("NumericFloat = %v, %v; want %v, success %t", value, err, test.want, test.ok)
			}
		})
	}
}

func TestInvalidJSONNumbers(t *testing.T) {
	for _, number := range []json.Number{"", "NaN", "Inf", "+1", "01", "0x10", "1/2", "1_000", "1.", ".1", "1e", " 1", "1 ", "null", "true", `"1"`, "[]", "1e99999999999999999999"} {
		t.Run(string(number), func(t *testing.T) {
			if _, err := ExactSigned(number); err == nil {
				t.Fatal("ExactSigned accepted invalid JSON number")
			}
			if _, err := ExactUnsigned(number); err == nil {
				t.Fatal("ExactUnsigned accepted invalid JSON number")
			}
			if _, err := NumericFloat(number); err == nil {
				t.Fatal("NumericFloat accepted invalid JSON number")
			}
		})
	}
}
