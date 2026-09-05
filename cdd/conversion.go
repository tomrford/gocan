package cdd

import (
	"fmt"
	"math"
	"strconv"

	"github.com/tomrford/gocan/internal/scalar"
)

// CANdelaStudio 9.1 and 17 English help specify physical = (f/div)*raw + o,
// with div defaulting to 1. These rules were transcribed from installed Vector
// help; the exact 10.0.108 DTD was not available. Missing f/o remain unsupported
// rather than acquiring invented defaults. Zero f needs an explicit inverse,
// which the codec does not implement.
func parseConversion(comp *element, signed bool) (*LinearConversion, error) {
	parameters := [3]float64{0, 0, 1}
	for index, name := range []string{"f", "o", "div"} {
		value, present := comp.attrs[name]
		if !present {
			if name == "div" {
				continue
			}
			return nil, fmt.Errorf("linear conversion without %s is unsupported", name)
		}
		number, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, fmt.Errorf("linear conversion has invalid %s %q", name, value)
		}
		parameters[index] = number
	}
	factor, offset, divisor := parameters[0], parameters[1], parameters[2]
	if divisor == 0 {
		return nil, fmt.Errorf("linear conversion has zero divisor")
	}
	scale := factor / divisor
	if math.IsInf(scale, 0) || factor != 0 && scale == 0 {
		return nil, fmt.Errorf("linear conversion scale is not representable")
	}
	conversion := &LinearConversion{Scale: scale, Offset: offset}
	for _, bound := range []struct {
		name        string
		destination **uint64
	}{{"s", &conversion.minimum}, {"e", &conversion.maximum}} {
		if value, present := comp.attrs[bound.name]; present {
			var key uint64
			var err error
			if signed {
				var integer int64
				integer, err = strconv.ParseInt(value, 10, 64)
				key = uint64(integer) ^ (uint64(1) << 63)
			} else {
				key, err = strconv.ParseUint(value, 10, 64)
			}
			if err != nil {
				return nil, fmt.Errorf("linear conversion has invalid raw limit %s %q", bound.name, value)
			}
			*bound.destination = &key
		}
	}
	if conversion.minimum != nil && conversion.maximum != nil && *conversion.minimum > *conversion.maximum {
		return nil, fmt.Errorf("linear conversion raw limits are reversed")
	}
	return conversion, nil
}

func validateConversionRaw(field Field, raw uint64) error {
	conversion := field.Conversion
	if conversion == nil {
		return nil
	}
	key := raw
	if field.Encoding == EncodingSigned {
		key = uint64(scalar.DecodeSigned(field.BitLength, raw)) ^ (uint64(1) << 63)
	}
	if conversion.minimum != nil && key < *conversion.minimum || conversion.maximum != nil && key > *conversion.maximum {
		return fmt.Errorf("raw value is outside linear conversion limits")
	}
	return nil
}

func validateConversionRange(field Field) error {
	minimum, maximum := uint64(0), uint64(math.MaxUint64)
	if field.BitLength < 64 {
		maximum = uint64(1)<<field.BitLength - 1
		if field.Encoding == EncodingSigned {
			half := uint64(1) << (field.BitLength - 1)
			minimum, maximum = uint64(1)<<63-half, uint64(1)<<63+half-1
		}
	}
	conversion := field.Conversion
	if conversion.minimum != nil && *conversion.minimum > maximum || conversion.maximum != nil && *conversion.maximum < minimum {
		return fmt.Errorf("linear conversion limits contain no representable raw values")
	}
	return nil
}
