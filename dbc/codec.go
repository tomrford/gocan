package dbc

import (
	"fmt"
	"math"
	"reflect"

	"github.com/tomrford/gocan"
)

type messageCodec struct {
	signalsByName map[string]int
	constraints   []map[int][]MultiplexRange
	selectors     []bool
	err           error
	writeErr      error
}

// MessageByName returns the resolved message with name.
func (database *Database) MessageByName(name string) (*Message, bool) {
	if database == nil {
		return nil, false
	}
	index, ok := database.messagesByName[name]
	if !ok {
		return nil, false
	}
	return &database.Messages[index], true
}

// Encode constructs a complete raw frame from the values of every signal
// active on the selected multiplexing path.
func (message *Message) Encode(values Values) (gocan.Frame, error) {
	codec, err := message.writableCodec()
	if err != nil {
		return gocan.Frame{}, err
	}
	raw, provided, err := codec.convertValues(message, values)
	if err != nil {
		return gocan.Frame{}, err
	}
	active, err := codec.activeSignals(message, raw)
	if err != nil {
		return gocan.Frame{}, err
	}
	for index, signal := range message.Signals {
		_, hasValue := provided[index]
		switch {
		case active[index] && !hasValue:
			return gocan.Frame{}, fmt.Errorf("DBC message %q requires signal %q", message.Name, signal.Name)
		case !active[index] && hasValue:
			return gocan.Frame{}, fmt.Errorf("DBC signal %q is inactive for the selected multiplexing path", signal.Name)
		}
	}

	frame, err := message.newFrame()
	if err != nil {
		return gocan.Frame{}, err
	}
	for index := range message.Signals {
		if active[index] {
			writeSignalBits(&frame.Data, message.Signals[index], raw[index])
		}
	}
	return frame, nil
}

// Patch applies changes to frame. Signals omitted from changes retain their
// existing raw bits. Changing a multiplexing path requires values for every
// signal that becomes active on the new path. Patch validates all changes
// before modifying frame.
func (message *Message) Patch(frame *gocan.Frame, changes Values) error {
	codec, err := message.writableCodec()
	if err != nil {
		return err
	}
	if frame == nil {
		return fmt.Errorf("DBC message %q cannot patch a nil frame", message.Name)
	}
	if err := message.validateFrame(*frame); err != nil {
		return err
	}
	changedRaw, changed, err := codec.convertValues(message, changes)
	if err != nil {
		return err
	}

	oldSelectors := codec.selectorValues(message, frame)
	newSelectors := make(map[int]uint64, len(oldSelectors))
	for index, value := range oldSelectors {
		newSelectors[index] = value
	}
	for index, value := range changedRaw {
		if codec.selectors[index] {
			newSelectors[index] = value
		}
	}
	oldActive, err := codec.activeSignals(message, oldSelectors)
	if err != nil {
		return err
	}
	newActive, err := codec.activeSignals(message, newSelectors)
	if err != nil {
		return err
	}

	// A selector which has just become active must be supplied before its
	// retained, previously inactive bits can select a child path.
	for index, signal := range message.Signals {
		if codec.selectors[index] && newActive[index] && !oldActive[index] {
			if _, ok := changed[index]; !ok {
				return fmt.Errorf("DBC multiplexor %q became active and requires a value", signal.Name)
			}
		}
	}
	for index, signal := range message.Signals {
		_, isChanged := changed[index]
		switch {
		case newActive[index] && !oldActive[index] && !isChanged:
			return fmt.Errorf("DBC signal %q became active and requires a value", signal.Name)
		case !newActive[index] && isChanged:
			return fmt.Errorf("DBC signal %q is inactive for the selected multiplexing path", signal.Name)
		}
	}

	for index := range changed {
		writeSignalBits(&frame.Data, message.Signals[index], changedRaw[index])
	}
	return nil
}

// Decode returns the physical value of one named signal from frame. It reads
// only that signal and the multiplexors needed to establish whether it is
// active.
func (message *Message) Decode(frame gocan.Frame, name string) (any, error) {
	codec, err := message.usableCodec()
	if err != nil {
		return nil, err
	}
	if err := message.validateFrame(frame); err != nil {
		return nil, err
	}
	index, ok := codec.signalsByName[name]
	if !ok {
		return nil, fmt.Errorf("DBC message %q has no signal %q", message.Name, name)
	}
	selectors := make(map[int]uint64)
	codec.readSelectorPath(message, &frame, index, selectors)
	active, err := codec.signalActive(message, index, selectors, make(map[int]bool))
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, fmt.Errorf("DBC signal %q is inactive in this frame", name)
	}
	return decodeSignalValue(message.Signals[index], readSignalBits(&frame.Data, message.Signals[index])), nil
}

func (message *Message) usableCodec() (*messageCodec, error) {
	if message == nil {
		return nil, fmt.Errorf("DBC message is nil")
	}
	if message.codec == nil {
		return nil, fmt.Errorf("DBC message %q has no resolved codec; obtain messages from Parse", message.Name)
	}
	if message.codec.err != nil {
		return nil, fmt.Errorf("DBC message %q cannot be encoded or decoded: %w", message.Name, message.codec.err)
	}
	return message.codec, nil
}

func (message *Message) writableCodec() (*messageCodec, error) {
	codec, err := message.usableCodec()
	if err != nil {
		return nil, err
	}
	if codec.writeErr != nil {
		return nil, fmt.Errorf("DBC message %q cannot be encoded or patched: %w", message.Name, codec.writeErr)
	}
	return codec, nil
}

func compileMessageCodec(message *Message) *messageCodec {
	codec := &messageCodec{
		signalsByName: make(map[string]int, len(message.Signals)),
		constraints:   make([]map[int][]MultiplexRange, len(message.Signals)),
		selectors:     make([]bool, len(message.Signals)),
	}
	for index, signal := range message.Signals {
		codec.signalsByName[signal.Name] = index
		codec.selectors[index] = signal.IsMultiplexer
		if signal.Factor == 0 && codec.writeErr == nil {
			codec.writeErr = fmt.Errorf("signal %q has a zero factor", signal.Name)
		}
		if signal.Minimum > signal.Maximum && codec.writeErr == nil {
			codec.writeErr = fmt.Errorf("signal %q has minimum greater than maximum", signal.Name)
		}
	}
	for index := range message.Signals {
		constraints, err := codec.signalConstraints(message, index, make(map[int]bool))
		if err != nil {
			codec.err = err
			return codec
		}
		codec.constraints[index] = constraints
	}
	for index, isSelector := range codec.selectors {
		if !isSelector {
			continue
		}
		signal := message.Signals[index]
		if signal.ValueType != ValueTypeInteger || signal.Signed {
			if codec.writeErr == nil {
				codec.writeErr = fmt.Errorf("multiplexor %q must be an unsigned integer signal", signal.Name)
			}
		}
	}
	if err := codec.validateOverlaps(message); err != nil && codec.writeErr == nil {
		codec.writeErr = err
	}
	return codec
}

func (codec *messageCodec) signalConstraints(message *Message, index int, visiting map[int]bool) (map[int][]MultiplexRange, error) {
	if visiting[index] {
		return nil, fmt.Errorf("multiplexer cycle includes signal %q", message.Signals[index].Name)
	}
	condition := message.Signals[index].Multiplex
	if condition == nil {
		return make(map[int][]MultiplexRange), nil
	}
	selector, ok := codec.signalsByName[condition.Selector]
	if !ok {
		return nil, fmt.Errorf("signal %q references unknown multiplexor %q", message.Signals[index].Name, condition.Selector)
	}
	visiting[index] = true
	constraints, err := codec.signalConstraints(message, selector, visiting)
	delete(visiting, index)
	if err != nil {
		return nil, err
	}
	result := make(map[int][]MultiplexRange, len(constraints)+1)
	for parent, ranges := range constraints {
		result[parent] = ranges
	}
	result[selector] = condition.Ranges
	return result, nil
}

func (codec *messageCodec) validateOverlaps(message *Message) error {
	masks := make([][8]uint64, len(message.Signals))
	for index, signal := range message.Signals {
		visitSignalBits(signal, func(bit uint32) {
			masks[index][bit/64] |= uint64(1) << (bit % 64)
		})
	}
	for left := range message.Signals {
		for right := left + 1; right < len(message.Signals); right++ {
			if !constraintsCompatible(codec.constraints[left], codec.constraints[right]) {
				continue
			}
			for word := range masks[left] {
				if masks[left][word]&masks[right][word] != 0 {
					return fmt.Errorf("signals %q and %q overlap while active", message.Signals[left].Name, message.Signals[right].Name)
				}
			}
		}
	}
	return nil
}

func constraintsCompatible(left, right map[int][]MultiplexRange) bool {
	for selector, leftRanges := range left {
		rightRanges, shared := right[selector]
		if shared && !rangesIntersect(leftRanges, rightRanges) {
			return false
		}
	}
	return true
}

func rangesIntersect(left, right []MultiplexRange) bool {
	for _, a := range left {
		for _, b := range right {
			if a.First <= b.Last && b.First <= a.Last {
				return true
			}
		}
	}
	return false
}

func (codec *messageCodec) convertValues(message *Message, values Values) (map[int]uint64, map[int]struct{}, error) {
	raw := make(map[int]uint64, len(values))
	provided := make(map[int]struct{}, len(values))
	for name, value := range values {
		index, ok := codec.signalsByName[name]
		if !ok {
			return nil, nil, fmt.Errorf("DBC message %q has no signal %q", message.Name, name)
		}
		encoded, err := encodeSignalValue(message.Signals[index], value)
		if err != nil {
			return nil, nil, fmt.Errorf("encode DBC signal %q: %w", name, err)
		}
		raw[index] = encoded
		provided[index] = struct{}{}
	}
	return raw, provided, nil
}

func (codec *messageCodec) activeSignals(message *Message, raw map[int]uint64) ([]bool, error) {
	active := make([]bool, len(message.Signals))
	for index := range message.Signals {
		value, err := codec.signalActive(message, index, raw, make(map[int]bool))
		if err != nil {
			return nil, err
		}
		active[index] = value
	}
	return active, nil
}

func (codec *messageCodec) signalActive(message *Message, index int, raw map[int]uint64, visiting map[int]bool) (bool, error) {
	condition := message.Signals[index].Multiplex
	if condition == nil {
		return true, nil
	}
	if visiting[index] {
		return false, fmt.Errorf("multiplexer cycle includes signal %q", message.Signals[index].Name)
	}
	selector := codec.signalsByName[condition.Selector]
	visiting[index] = true
	selectorActive, err := codec.signalActive(message, selector, raw, visiting)
	delete(visiting, index)
	if err != nil || !selectorActive {
		return false, err
	}
	value, ok := raw[selector]
	if !ok {
		return false, fmt.Errorf("DBC message %q requires multiplexor %q", message.Name, message.Signals[selector].Name)
	}
	return rangesContain(condition.Ranges, value), nil
}

func (codec *messageCodec) selectorValues(message *Message, frame *gocan.Frame) map[int]uint64 {
	values := make(map[int]uint64)
	for index, isSelector := range codec.selectors {
		if isSelector {
			values[index] = readSignalBits(&frame.Data, message.Signals[index])
		}
	}
	return values
}

func (codec *messageCodec) readSelectorPath(message *Message, frame *gocan.Frame, index int, values map[int]uint64) {
	condition := message.Signals[index].Multiplex
	if condition == nil {
		return
	}
	selector := codec.signalsByName[condition.Selector]
	codec.readSelectorPath(message, frame, selector, values)
	values[selector] = readSignalBits(&frame.Data, message.Signals[selector])
}

func (message *Message) newFrame() (gocan.Frame, error) {
	dlc, flags, err := message.frameShape()
	if err != nil {
		return gocan.Frame{}, err
	}
	return gocan.Frame{ID: message.ID, DLC: dlc, Flags: flags}, nil
}

func (message *Message) validateFrame(frame gocan.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	dlc, flags, err := message.frameShape()
	if err != nil {
		return err
	}
	if frame.Flags.Has(gocan.FrameRemote) {
		return fmt.Errorf("DBC message %q cannot decode a remote frame", message.Name)
	}
	if frame.ID != message.ID || frame.Flags.Has(gocan.FrameExtended) != flags.Has(gocan.FrameExtended) {
		return fmt.Errorf("frame %#x does not match DBC message %q", frame.ID, message.Name)
	}
	if frame.Flags.Has(gocan.FrameFD) != flags.Has(gocan.FrameFD) {
		return fmt.Errorf("frame format does not match DBC message %q", message.Name)
	}
	expectedLength, err := gocan.DLCToLength(dlc, flags.Has(gocan.FrameFD))
	if err != nil {
		return err
	}
	if frame.DataLength() != expectedLength {
		return fmt.Errorf("frame payload length %d does not match DBC message %q length %d", frame.DataLength(), message.Name, expectedLength)
	}
	return nil
}

func (message *Message) frameShape() (uint8, gocan.FrameFlags, error) {
	var flags gocan.FrameFlags
	if message.Extended {
		flags |= gocan.FrameExtended
	}
	length := int(message.Length)
	switch message.Format {
	case FrameFormatStandardCAN, FrameFormatExtendedCAN:
		if length > 8 {
			return 0, 0, fmt.Errorf("DBC message %q exceeds classical CAN length", message.Name)
		}
	case FrameFormatStandardCANFD, FrameFormatExtendedCANFD:
		flags |= gocan.FrameFD
		length = legalFDLength(length)
	case FrameFormatJ1939:
		if length > 8 {
			return 0, 0, fmt.Errorf("DBC J1939 message %q exceeds one classical CAN frame", message.Name)
		}
	default:
		return 0, 0, fmt.Errorf("DBC message %q has no supported frame format", message.Name)
	}
	if message.bitRateSwitch {
		if !flags.Has(gocan.FrameFD) {
			return 0, 0, fmt.Errorf("DBC message %q requests bit-rate switching without CAN FD", message.Name)
		}
		flags |= gocan.FrameBitRateSwitch
	}
	dlc, err := gocan.LengthToDLC(length, flags.Has(gocan.FrameFD))
	if err != nil {
		return 0, 0, fmt.Errorf("DBC message %q: %w", message.Name, err)
	}
	return dlc, flags, nil
}

func legalFDLength(length int) int {
	for _, candidate := range [...]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 20, 24, 32, 48, 64} {
		if length <= candidate {
			return candidate
		}
	}
	return length
}

func encodeSignalValue(signal Signal, value any) (uint64, error) {
	if label, ok := stringValue(value); ok {
		if signal.ValueType != ValueTypeInteger {
			return 0, fmt.Errorf("value descriptions require an integer signal")
		}
		raw, err := rawForLabel(signal, label)
		if err != nil {
			return 0, err
		}
		return validateIntegerRaw(signal, raw)
	}
	if boolean, ok := boolValue(value); ok {
		if signal.ValueType != ValueTypeInteger || signal.BitLength != 1 || signal.Factor != 1 || signal.Offset != 0 {
			return 0, fmt.Errorf("bool requires an unscaled one-bit integer signal")
		}
		physical := 0.0
		if boolean {
			physical = 1
		}
		if err := validatePhysical(signal, physical); err != nil {
			return 0, err
		}
		return uint64(physical), nil
	}
	if signal.ValueType == ValueTypeFloat32 || signal.ValueType == ValueTypeFloat64 {
		physical, err := numericFloat(value)
		if err != nil {
			return 0, err
		}
		if err := validatePhysical(signal, physical); err != nil {
			return 0, err
		}
		raw := (physical - signal.Offset) / signal.Factor
		if math.IsNaN(raw) || math.IsInf(raw, 0) {
			return 0, fmt.Errorf("value does not produce a finite raw float")
		}
		if signal.ValueType == ValueTypeFloat32 {
			raw32 := float32(raw)
			if math.IsInf(float64(raw32), 0) {
				return 0, fmt.Errorf("value exceeds float32")
			}
			return uint64(math.Float32bits(raw32)), nil
		}
		return math.Float64bits(raw), nil
	}

	if signal.Factor == 1 && signal.Offset == 0 {
		if signal.Signed {
			value, err := exactSigned(value)
			if err != nil {
				return 0, err
			}
			if err := validatePhysical(signal, float64(value)); err != nil {
				return 0, err
			}
			return encodeSigned(signal.BitLength, value)
		}
		value, err := exactUnsigned(value)
		if err != nil {
			return 0, err
		}
		if err := validatePhysical(signal, float64(value)); err != nil {
			return 0, err
		}
		return encodeUnsigned(signal.BitLength, value)
	}

	physical, err := numericFloat(value)
	if err != nil {
		return 0, err
	}
	if err := validatePhysical(signal, physical); err != nil {
		return 0, err
	}
	rawFloat := (physical - signal.Offset) / signal.Factor
	rounded := math.Round(rawFloat)
	tolerance := math.Max(1e-9, 2*math.Abs(math.Nextafter(rawFloat, math.Inf(1))-rawFloat))
	if math.IsNaN(rawFloat) || math.IsInf(rawFloat, 0) || math.Abs(rawFloat-rounded) > tolerance {
		return 0, fmt.Errorf("physical value %v does not map to an integer raw value", physical)
	}
	if signal.Signed {
		if rounded < -math.Exp2(63) || rounded >= math.Exp2(63) {
			return 0, fmt.Errorf("raw value %v exceeds int64", rounded)
		}
		return encodeSigned(signal.BitLength, int64(rounded))
	}
	if rounded < 0 || rounded >= math.Exp2(64) {
		return 0, fmt.Errorf("raw value %v exceeds uint64", rounded)
	}
	return encodeUnsigned(signal.BitLength, uint64(rounded))
}

func decodeSignalValue(signal Signal, raw uint64) any {
	if signal.ValueType == ValueTypeFloat32 {
		return float64(math.Float32frombits(uint32(raw)))*signal.Factor + signal.Offset
	}
	if signal.ValueType == ValueTypeFloat64 {
		return math.Float64frombits(raw)*signal.Factor + signal.Offset
	}
	if signal.Signed {
		value := decodeSigned(signal.BitLength, raw)
		if signal.Factor == 1 && signal.Offset == 0 {
			return value
		}
		return float64(value)*signal.Factor + signal.Offset
	}
	if signal.Factor == 1 && signal.Offset == 0 {
		return raw
	}
	return float64(raw)*signal.Factor + signal.Offset
}

func rawForLabel(signal Signal, label string) (uint64, error) {
	found := false
	var value int64
	for _, description := range signal.Values {
		if description.Label != label {
			continue
		}
		if found && value != description.Value {
			return 0, fmt.Errorf("value description %q is ambiguous", label)
		}
		found = true
		value = description.Value
	}
	if !found {
		return 0, fmt.Errorf("unknown value description %q", label)
	}
	if signal.Signed {
		return encodeSigned(signal.BitLength, value)
	}
	if value < 0 {
		return 0, fmt.Errorf("negative value description %d for unsigned signal", value)
	}
	return encodeUnsigned(signal.BitLength, uint64(value))
}

func validateIntegerRaw(signal Signal, raw uint64) (uint64, error) {
	physical := float64(raw)*signal.Factor + signal.Offset
	if signal.Signed {
		physical = float64(decodeSigned(signal.BitLength, raw))*signal.Factor + signal.Offset
	}
	if err := validatePhysical(signal, physical); err != nil {
		return 0, err
	}
	return raw, nil
}

func validatePhysical(signal Signal, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("physical value must be finite")
	}
	// DBC writers commonly use [0|0] when physical limits are unavailable.
	if signal.Minimum == 0 && signal.Maximum == 0 {
		return nil
	}
	if value < signal.Minimum || value > signal.Maximum {
		return fmt.Errorf("physical value %v is outside [%v, %v]", value, signal.Minimum, signal.Maximum)
	}
	return nil
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
	return uint64(value) & ((uint64(1) << bits) - 1), nil
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
	mask := (uint64(1) << bits) - 1
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
		unsigned := reflected.Uint()
		if unsigned > math.MaxInt64 {
			return 0, fmt.Errorf("unsigned value %d exceeds int64", unsigned)
		}
		return int64(unsigned), nil
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
		signed := reflected.Int()
		if signed < 0 {
			return 0, fmt.Errorf("negative value %d for unsigned signal", signed)
		}
		return uint64(signed), nil
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
		return float64(reflected.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(reflected.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return reflected.Float(), nil
	default:
		return 0, fmt.Errorf("value has unsupported type %T", value)
	}
}

func stringValue(value any) (string, bool) {
	reflected := reflect.ValueOf(value)
	if reflected.IsValid() && reflected.Kind() == reflect.String {
		return reflected.String(), true
	}
	return "", false
}

func boolValue(value any) (bool, bool) {
	reflected := reflect.ValueOf(value)
	if reflected.IsValid() && reflected.Kind() == reflect.Bool {
		return reflected.Bool(), true
	}
	return false, false
}

func readSignalBits(data *[gocan.MaxDataLength]byte, signal Signal) uint64 {
	var value uint64
	if signal.ByteOrder == ByteOrderLittleEndian {
		for valueBit := uint32(0); valueBit < signal.BitLength; valueBit++ {
			dataBit := signal.StartBit + valueBit
			if data[dataBit/8]&(byte(1)<<(dataBit%8)) != 0 {
				value |= uint64(1) << valueBit
			}
		}
		return value
	}
	dataBit := signal.StartBit
	for valueBit := uint32(0); valueBit < signal.BitLength; valueBit++ {
		value <<= 1
		if data[dataBit/8]&(byte(1)<<(dataBit%8)) != 0 {
			value |= 1
		}
		dataBit = nextBigEndianBit(dataBit)
	}
	return value
}

func writeSignalBits(data *[gocan.MaxDataLength]byte, signal Signal, value uint64) {
	if signal.ByteOrder == ByteOrderLittleEndian {
		for valueBit := uint32(0); valueBit < signal.BitLength; valueBit++ {
			dataBit := signal.StartBit + valueBit
			setDataBit(data, dataBit, value&(uint64(1)<<valueBit) != 0)
		}
		return
	}
	dataBit := signal.StartBit
	for valueBit := uint32(0); valueBit < signal.BitLength; valueBit++ {
		shift := signal.BitLength - 1 - valueBit
		setDataBit(data, dataBit, value&(uint64(1)<<shift) != 0)
		dataBit = nextBigEndianBit(dataBit)
	}
}

func setDataBit(data *[gocan.MaxDataLength]byte, bit uint32, value bool) {
	mask := byte(1) << (bit % 8)
	if value {
		data[bit/8] |= mask
	} else {
		data[bit/8] &^= mask
	}
}

func visitSignalBits(signal Signal, visit func(uint32)) {
	if signal.ByteOrder == ByteOrderLittleEndian {
		for index := uint32(0); index < signal.BitLength; index++ {
			visit(signal.StartBit + index)
		}
		return
	}
	bit := signal.StartBit
	for index := uint32(0); index < signal.BitLength; index++ {
		visit(bit)
		bit = nextBigEndianBit(bit)
	}
}

func nextBigEndianBit(bit uint32) uint32 {
	if bit%8 == 0 {
		return bit + 15
	}
	return bit - 1
}
