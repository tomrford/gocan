package scalar

import (
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
