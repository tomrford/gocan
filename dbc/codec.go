package dbc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/internal/scalar"
)

var (
	// ErrInactiveSignal means the selected multiplexing path excludes the
	// signal from this frame. It is an expected absence, not a
	// malformed-payload error.
	ErrInactiveSignal = errors.New("dbc: inactive signal")
	// ErrUnknownSignal means a signal name is not declared by the message.
	ErrUnknownSignal = errors.New("dbc: unknown signal")
	// ErrMissingValue means an encode or patch omitted a signal that is active
	// on the selected multiplexing path, or a multiplexor needed to select it.
	ErrMissingValue = errors.New("dbc: missing value")
	// ErrInvalidValue means a supplied value cannot be encoded for its signal:
	// it has an unsupported type, is outside the declared physical range, does
	// not fit the raw width, or names an unknown value description.
	ErrInvalidValue = errors.New("dbc: invalid value")
	// ErrFrameMismatch means a frame or payload does not match the message's
	// identifier, frame format or length, or is too short to carry a requested
	// signal or the multiplexors that select it.
	ErrFrameMismatch = errors.New("dbc: frame does not match message")
	// ErrUnsupportedMessage means the codec cannot perform the operation on
	// this message definition, such as a signal wider than 64 bits, a frame
	// operation on a message longer than one frame, or an encode on a message
	// whose definition cannot be written consistently.
	ErrUnsupportedMessage = errors.New("dbc: unsupported message")
	// ErrUnrepresentable means Write cannot express part of the model as DBC
	// text, such as an out-of-range identifier or an invalid name or string.
	ErrUnrepresentable = errors.New("dbc: not representable as DBC")
)

type messageCodec struct {
	signalsByName map[string]int
	constraints   []map[int][]MultiplexRange
	selectors     []bool
	choices       [][]scalar.Choice
	err           error
	writeErr      error
}

// MessageByName returns the resolved message with name.
func (db *Database) MessageByName(name string) (*Message, bool) {
	if db == nil {
		return nil, false
	}
	index, ok := db.messagesByName[name]
	if !ok {
		return nil, false
	}
	return &db.Messages[index], true
}

// MessageByID returns the resolved message with arbitration identifier id,
// which is 11-bit unless extended is set. The extended flag is part of the
// identity, so an extended message never answers a standard lookup of the same
// numeric identifier.
//
// A J1939 message matches only its declared identifier, including priority and
// source, so a frame from another source misses here even though Decode accepts
// it. Use MessagesByPGN to match J1939 traffic by parameter group.
func (db *Database) MessageByID(id uint32, extended bool) (*Message, bool) {
	if db == nil {
		return nil, false
	}
	index, ok := db.messagesByID[messageID{id: id, extended: extended}]
	if !ok {
		return nil, false
	}
	return &db.Messages[index], true
}

// MessagesByPGN returns the resolved J1939 messages with the canonical 18-bit
// parameter group number pgn, as j1939.Header.PGN reports it, in declaration
// order. More than one message may deliberately describe the same PGN.
// Callers must resolve multiple matches from their device/database context;
// this lookup does not pick a definition based on source or destination.
func (db *Database) MessagesByPGN(pgn uint32) []*Message {
	if db == nil {
		return nil
	}
	indexes := db.messagesByPGN[pgn]
	messages := make([]*Message, len(indexes))
	for i, index := range indexes {
		messages[i] = &db.Messages[index]
	}
	return messages
}

// SignalByName returns the resolved signal with name.
func (message *Message) SignalByName(name string) (*Signal, bool) {
	if message == nil {
		return nil, false
	}
	// The codec index describes the parsed signals. Confirming the name keeps
	// the lookup correct on a copy whose signals an Edit callback changed.
	if message.codec != nil {
		if index, ok := message.codec.signalsByName[name]; ok && index < len(message.Signals) && message.Signals[index].Name == name {
			return &message.Signals[index], true
		}
	}
	for index := range message.Signals {
		if message.Signals[index].Name == name {
			return &message.Signals[index], true
		}
	}
	return nil, false
}

// Encode constructs a complete raw frame from the values of every signal
// active on the selected multiplexing path. A scaled physical value is
// quantized to the nearest raw value, so Decode can differ from the encoded
// input by up to half a scale step. The message must fit one CAN frame; use
// EncodePayload for transported messages. Signals wider than 64 bits are not
// supported by the codec.
func (message *Message) Encode(values Values) (gocan.Frame, error) {
	codec, err := message.writableCodec()
	if err != nil {
		return gocan.Frame{}, err
	}
	frame, err := message.newFrame()
	if err != nil {
		return gocan.Frame{}, err
	}
	if err := codec.encode(message, frame.Data[:frame.DataLength()], values); err != nil {
		return gocan.Frame{}, err
	}
	return frame, nil
}

// EncodePayload encodes like Encode but returns exactly Length bytes,
// with unused bits zeroed, for messages carried by a transport.
func (message *Message) EncodePayload(values Values) ([]byte, error) {
	codec, err := message.writableCodec()
	if err != nil {
		return nil, err
	}
	if uint64(message.Length) > uint64(math.MaxInt) {
		return nil, fmt.Errorf("%w: message %q payload length %d exceeds platform capacity", ErrUnsupportedMessage, message.Name, message.Length)
	}
	payload := make([]byte, message.Length)
	if err := codec.encode(message, payload, values); err != nil {
		return nil, err
	}
	return payload, nil
}

func (codec *messageCodec) encode(message *Message, data []byte, values Values) error {
	raw, provided, err := codec.convertValues(message, values)
	if err != nil {
		return err
	}
	active, err := codec.activeSignals(message, raw)
	if err != nil {
		return err
	}
	for index, signal := range message.Signals {
		_, hasValue := provided[index]
		switch {
		case active[index] && !hasValue:
			return fmt.Errorf("%w: message %q requires signal %q", ErrMissingValue, message.Name, signal.Name)
		case !active[index] && hasValue:
			return fmt.Errorf("%w: signal %q is inactive for the selected multiplexing path", ErrInactiveSignal, signal.Name)
		}
	}

	for index := range message.Signals {
		if active[index] {
			writeSignalBits(data, message.Signals[index], raw[index])
		}
	}
	return nil
}

// Patch applies changes to frame. Signals omitted from changes retain their
// existing raw bits. Changing a multiplexing path requires values for every
// signal that becomes active on the new path. Patch validates all changes
// before modifying frame. Messages longer than the 64-byte raw frame
// representation or containing a signal wider than 64 bits are not supported
// by the codec.
func (message *Message) Patch(frame *gocan.Frame, changes Values) error {
	codec, err := message.writableCodec()
	if err != nil {
		return err
	}
	if frame == nil {
		return fmt.Errorf("dbc: message %q cannot patch a nil frame", message.Name)
	}
	if err := message.validateFrame(*frame); err != nil {
		return err
	}
	changedRaw, changed, err := codec.convertValues(message, changes)
	if err != nil {
		return err
	}

	data := frame.Data[:frame.DataLength()]
	oldSelectors := codec.selectorValues(message, data)
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

	for index, signal := range message.Signals {
		if codec.selectors[index] && newActive[index] && !oldActive[index] {
			if _, ok := changed[index]; !ok {
				return fmt.Errorf("%w: multiplexor %q became active and requires a value", ErrMissingValue, signal.Name)
			}
		}
	}
	for index, signal := range message.Signals {
		_, isChanged := changed[index]
		switch {
		case newActive[index] && !oldActive[index] && !isChanged:
			return fmt.Errorf("%w: signal %q became active and requires a value", ErrMissingValue, signal.Name)
		case !newActive[index] && isChanged:
			return fmt.Errorf("%w: signal %q is inactive for the selected multiplexing path", ErrInactiveSignal, signal.Name)
		}
	}

	for index := range changed {
		writeSignalBits(data, message.Signals[index], changedRaw[index])
	}
	return nil
}

// Decode returns the physical value of one named signal from frame. A raw
// value carrying a value description decodes to its label; other values decode
// numerically. It reads only that signal and the multiplexors needed to
// establish whether it is active. The message must fit one CAN frame; use
// DecodePayload for transported messages. Signals wider than 64 bits are not
// supported by the codec.
// J1939 definitions match by canonical PGN, regardless of priority, source or
// destination. Select the appropriate definition first when a PGN is ambiguous.
// An inactive signal returns an error wrapping ErrInactiveSignal.
func (message *Message) Decode(frame gocan.Frame, name string) (any, error) {
	if err := message.validateFrame(frame); err != nil {
		return nil, err
	}
	return message.DecodePayload(frame.Data[:message.Length], name)
}

// DecodePayload returns the physical value of one named signal from a payload.
// The payload may be shorter than the declared message length. Only multiplexors
// needed to determine activity and the active signal's bits must be present.
// An inactive signal returns an error wrapping ErrInactiveSignal, even if its
// bits are absent. The payload must not exceed the declared message length.
func (message *Message) DecodePayload(payload []byte, name string) (any, error) {
	codec, err := message.usableCodec()
	if err != nil {
		return nil, err
	}
	if uint64(len(payload)) > uint64(message.Length) {
		return nil, fmt.Errorf("%w: message %q payload length %d exceeds declared length %d", ErrFrameMismatch, message.Name, len(payload), message.Length)
	}
	index, ok := codec.signalsByName[name]
	if !ok {
		return nil, fmt.Errorf("%w: message %q has no signal %q", ErrUnknownSignal, message.Name, name)
	}
	active, err := codec.signalActiveInPayload(message, payload, index)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, fmt.Errorf("%w: signal %q", ErrInactiveSignal, name)
	}
	if !signalAvailable(message.Signals[index], len(payload)) {
		return nil, fmt.Errorf("%w: signal %q is not present in the %d-byte payload", ErrFrameMismatch, name, len(payload))
	}
	return decodeSignalValue(message.Signals[index], codec.choices[index], readSignalBits(payload, message.Signals[index])), nil
}

func (message *Message) usableCodec() (*messageCodec, error) {
	if message == nil {
		return nil, fmt.Errorf("dbc: message is nil")
	}
	if message.codec == nil {
		return nil, fmt.Errorf("dbc: message %q has no resolved codec; obtain messages from Parse", message.Name)
	}
	if message.codec.err != nil {
		return nil, fmt.Errorf("%w: message %q cannot be encoded or decoded: %w", ErrUnsupportedMessage, message.Name, message.codec.err)
	}
	return message.codec, nil
}

func (message *Message) writableCodec() (*messageCodec, error) {
	codec, err := message.usableCodec()
	if err != nil {
		return nil, err
	}
	if codec.writeErr != nil {
		return nil, fmt.Errorf("%w: message %q cannot be encoded or patched: %w", ErrUnsupportedMessage, message.Name, codec.writeErr)
	}
	return codec, nil
}

func compileMessageCodec(message *Message) *messageCodec {
	codec := &messageCodec{
		signalsByName: make(map[string]int, len(message.Signals)),
		constraints:   make([]map[int][]MultiplexRange, len(message.Signals)),
		selectors:     make([]bool, len(message.Signals)),
		choices:       make([][]scalar.Choice, len(message.Signals)),
	}
	for index, signal := range message.Signals {
		if signal.BitLength > 64 {
			codec.err = fmt.Errorf("signal %q exceeds the 64-bit codec representation", signal.Name)
			return codec
		}
		if signal.ByteOrder != binary.BigEndian && signal.ByteOrder != binary.LittleEndian {
			codec.err = fmt.Errorf("signal %q has unsupported byte order %v", signal.Name, signal.ByteOrder)
			return codec
		}
		if len(signal.Values) > 0 {
			choices := make([]scalar.Choice, len(signal.Values))
			for i, value := range signal.Values {
				choices[i] = scalar.Choice{Value: value.Value, Label: value.Label}
			}
			codec.choices[index] = choices
		}
		codec.signalsByName[signal.Name] = index
		codec.selectors[index] = signal.IsMultiplexer
		if codec.writeErr == nil && (signal.Factor == 0 || math.IsNaN(signal.Factor) || math.IsInf(signal.Factor, 0) ||
			math.IsNaN(signal.Offset) || math.IsInf(signal.Offset, 0)) {
			codec.writeErr = fmt.Errorf("signal %q must have a finite nonzero factor and finite offset", signal.Name)
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
	// A signal of at most 64 bits touches at most two adjacent 64-bit words,
	// including Motorola bit ordering. Keep its absolute word offset separate
	// so sparse transported messages do not allocate by declared payload size.
	type signalMask struct {
		first uint32
		words [2]uint64
	}
	bits := make([]signalMask, len(message.Signals))
	for index, signal := range message.Signals {
		bits[index].first = signal.StartBit / 64
		signal.StartBit %= 64
		visitSignalBits(signal, func(bit uint32) {
			bits[index].words[bit/64] |= uint64(1) << (bit % 64)
		})
	}
	for left := range message.Signals {
		for right := left + 1; right < len(message.Signals); right++ {
			if !constraintsCompatible(codec.constraints[left], codec.constraints[right]) {
				continue
			}
			a, b := bits[left], bits[right]
			if a.first > b.first {
				a, b = b, a
			}
			distance := b.first - a.first
			if distance == 0 && (a.words[0]&b.words[0]|a.words[1]&b.words[1]) != 0 ||
				distance == 1 && a.words[1]&b.words[0] != 0 {
				return fmt.Errorf("signals %q and %q overlap while active", message.Signals[left].Name, message.Signals[right].Name)
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
			return nil, nil, fmt.Errorf("%w: message %q has no signal %q", ErrUnknownSignal, message.Name, name)
		}
		encoded, err := encodeSignalValue(message.Signals[index], codec.choices[index], value)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: signal %q: %w", ErrInvalidValue, name, err)
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
		return false, fmt.Errorf("dbc: multiplexer cycle includes signal %q", message.Signals[index].Name)
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
		return false, fmt.Errorf("%w: message %q requires multiplexor %q", ErrMissingValue, message.Name, message.Signals[selector].Name)
	}
	return rangesContain(condition.Ranges, value), nil
}

func (codec *messageCodec) selectorValues(message *Message, payload []byte) map[int]uint64 {
	values := make(map[int]uint64)
	for index, isSelector := range codec.selectors {
		if isSelector {
			values[index] = readSignalBits(payload, message.Signals[index])
		}
	}
	return values
}

func (codec *messageCodec) signalActiveInPayload(message *Message, payload []byte, index int) (bool, error) {
	condition := message.Signals[index].Multiplex
	if condition == nil {
		return true, nil
	}
	selector := codec.signalsByName[condition.Selector]
	active, err := codec.signalActiveInPayload(message, payload, selector)
	if err != nil || !active {
		return false, err
	}
	if !signalAvailable(message.Signals[selector], len(payload)) {
		return false, fmt.Errorf("%w: multiplexor %q is not present in the %d-byte payload", ErrFrameMismatch, message.Signals[selector].Name, len(payload))
	}
	return rangesContain(condition.Ranges, readSignalBits(payload, message.Signals[selector])), nil
}

func (message *Message) newFrame() (gocan.Frame, error) {
	dlc, flags, err := message.frameShape()
	if err != nil {
		return gocan.Frame{}, err
	}
	return gocan.Frame{ID: message.ID, DLC: dlc, Flags: flags}, nil
}

func (message *Message) validateFrame(frame gocan.Frame) error {
	if message == nil {
		return fmt.Errorf("dbc: message is nil")
	}
	if err := frame.Validate(); err != nil {
		return err
	}
	dlc, flags, err := message.frameShape()
	if err != nil {
		return err
	}
	if frame.Flags.Has(gocan.FrameRemote) {
		return fmt.Errorf("%w: message %q cannot decode a remote frame", ErrFrameMismatch, message.Name)
	}
	matchesID := frame.ID == message.ID
	if message.Format == FrameFormatJ1939 {
		matchesID = pgnOf(frame.ID) == pgnOf(message.ID)
	}
	if !matchesID || frame.Flags.Has(gocan.FrameExtended) != flags.Has(gocan.FrameExtended) {
		return fmt.Errorf("%w: frame %#x does not match message %q", ErrFrameMismatch, frame.ID, message.Name)
	}
	if frame.Flags.Has(gocan.FrameFD) != flags.Has(gocan.FrameFD) {
		return fmt.Errorf("%w: frame format does not match message %q", ErrFrameMismatch, message.Name)
	}
	expectedLength, err := gocan.DLCToLength(dlc, flags.Has(gocan.FrameFD))
	if err != nil {
		return err
	}
	if frame.DataLength() != expectedLength {
		return fmt.Errorf("%w: frame payload length %d does not match message %q length %d", ErrFrameMismatch, frame.DataLength(), message.Name, expectedLength)
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
			return 0, 0, fmt.Errorf("%w: message %q exceeds classical CAN length", ErrUnsupportedMessage, message.Name)
		}
	case FrameFormatStandardCANFD, FrameFormatExtendedCANFD:
		flags |= gocan.FrameFD
		length = legalFDLength(length)
	case FrameFormatJ1939:
		if length > 8 {
			return 0, 0, fmt.Errorf("%w: J1939 message %q exceeds one classical CAN frame", ErrUnsupportedMessage, message.Name)
		}
	default:
		return 0, 0, fmt.Errorf("%w: message %q has no supported frame format", ErrUnsupportedMessage, message.Name)
	}
	if message.bitRateSwitch {
		if !flags.Has(gocan.FrameFD) {
			return 0, 0, fmt.Errorf("%w: message %q requests bit-rate switching without CAN FD", ErrUnsupportedMessage, message.Name)
		}
		flags |= gocan.FrameBitRateSwitch
	}
	dlc, err := gocan.LengthToDLC(length, flags.Has(gocan.FrameFD))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: message %q: %w", ErrUnsupportedMessage, message.Name, err)
	}
	return dlc, flags, nil
}

// pgnOf returns the canonical J1939 parameter group number of a 29-bit
// identifier, as j1939.ParseID derives it: a PDU1 group drops its destination
// address.
func pgnOf(id uint32) uint32 {
	pgn := id >> 8 & 0x3ffff
	if uint8(pgn>>8) < 0xf0 {
		pgn &= 0x3ff00
	}
	return pgn
}

func legalFDLength(length int) int {
	for _, candidate := range [...]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 20, 24, 32, 48, 64} {
		if length <= candidate {
			return candidate
		}
	}
	return length
}

func encodeSignalValue(signal Signal, choices []scalar.Choice, value any) (uint64, error) {
	if label, ok := scalar.StringValue(value); ok {
		if signal.ValueType != ValueTypeInteger {
			return 0, fmt.Errorf("value descriptions require an integer signal")
		}
		// A value description is an exact raw value sanctioned by the catalog,
		// so it bypasses the physical range like the not-available idiom
		// requires. Decode returns the same label back.
		return scalar.RawForLabel(choices, label, signal.BitLength, signal.Signed)
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
		physical, err := scalar.NumericFloat(value, 64)
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
			if signal.Factor == 1 && signal.Offset == 0 {
				// Convert the original input directly to avoid double rounding.
				raw, err = scalar.NumericFloat(value, 32)
				if err != nil {
					return 0, err
				}
			}
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
			value, err := scalar.ExactSigned(value)
			if err != nil {
				return 0, err
			}
			if err := validatePhysical(signal, float64(value)); err != nil {
				return 0, err
			}
			return scalar.EncodeSigned(signal.BitLength, value)
		}
		value, err := scalar.ExactUnsigned(value)
		if err != nil {
			return 0, err
		}
		if err := validatePhysical(signal, float64(value)); err != nil {
			return 0, err
		}
		return scalar.EncodeUnsigned(signal.BitLength, value)
	}

	physical, err := scalar.LinearFloat(value)
	if err != nil {
		return 0, err
	}
	if err := validatePhysical(signal, physical); err != nil {
		return 0, err
	}
	raw, err := scalar.LinearRaw(signal.BitLength, signal.Signed, physical, signal.Factor, signal.Offset)
	if err != nil {
		return 0, err
	}
	// Quantization can move a boundary value across the declared range, so the
	// value that reaches the wire is validated again.
	return validateIntegerRaw(signal, raw)
}

func decodeSignalValue(signal Signal, choices []scalar.Choice, raw uint64) any {
	if signal.ValueType == ValueTypeFloat32 {
		return float64(math.Float32frombits(uint32(raw)))*signal.Factor + signal.Offset
	}
	if signal.ValueType == ValueTypeFloat64 {
		return math.Float64frombits(raw)*signal.Factor + signal.Offset
	}
	return scalar.DecodeLinear(signal.BitLength, signal.Signed, raw, signal.Factor, signal.Offset, choices, true)
}

func validateIntegerRaw(signal Signal, raw uint64) (uint64, error) {
	physical := float64(raw)*signal.Factor + signal.Offset
	if signal.Signed {
		physical = float64(scalar.DecodeSigned(signal.BitLength, raw))*signal.Factor + signal.Offset
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

func boolValue(value any) (bool, bool) {
	if boolean, ok := value.(bool); ok {
		return boolean, true
	}
	reflected := reflect.ValueOf(value)
	if reflected.IsValid() && reflected.Kind() == reflect.Bool {
		return reflected.Bool(), true
	}
	return false, false
}

func readSignalBits(data []byte, signal Signal) uint64 {
	var value uint64
	if signal.ByteOrder == binary.LittleEndian {
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

func writeSignalBits(data []byte, signal Signal, value uint64) {
	if signal.ByteOrder == binary.LittleEndian {
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

func setDataBit(data []byte, bit uint32, value bool) {
	mask := byte(1) << (bit % 8)
	if value {
		data[bit/8] |= mask
	} else {
		data[bit/8] &^= mask
	}
}

func signalAvailable(signal Signal, payloadLength int) bool {
	available := true
	payloadBits := uint64(payloadLength) * 8
	visitSignalBits(signal, func(bit uint32) {
		if uint64(bit) >= payloadBits {
			available = false
		}
	})
	return available
}

func visitSignalBits(signal Signal, visit func(uint32)) {
	if signal.ByteOrder == binary.LittleEndian {
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
