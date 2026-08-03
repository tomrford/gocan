package dbc

import (
	"fmt"
	"strconv"
	"strings"
)

// vectorIndependentSignalsID is the reserved identifier of the
// VECTOR__INDEPENDENT_SIG_MSG pseudo-message that CANdb++ exports orphan
// signals into. The pseudo-message and references to it are dropped.
const vectorIndependentSignalsID = 0xc0000000

type resolver struct {
	raw rawDatabase
	db  *Database

	nodesByName      map[string]int
	messagesByRawID  map[uint32]int
	messagesByName   map[string]int
	droppedMessages  map[uint32]bool
	signalsByMessage []map[string]int
	messagePositions []Position
	signalPositions  []map[string]Position
	definitions      map[string]int
	simpleMuxValues  []map[string]uint64
}

func resolve(raw rawDatabase) (*Database, error) {
	r := resolver{
		raw: raw,
		db: &Database{
			Version:     raw.version,
			Diagnostics: append([]Diagnostic(nil), raw.diagnostics...),
		},
		nodesByName:     make(map[string]int),
		messagesByRawID: make(map[uint32]int),
		messagesByName:  make(map[string]int),
		droppedMessages: make(map[uint32]bool),
		definitions:     make(map[string]int),
	}

	steps := []func() error{
		r.resolveNodes,
		r.resolveMessages,
		r.resolveValueTables,
		r.resolveAttributeDefinitions,
		r.resolveAttributeValues,
		r.resolveComments,
		r.resolveTransmitters,
		r.resolveValueLists,
		r.resolveValueTypes,
		r.resolveSignalGroups,
		r.resolveMultiplexing,
		r.resolveFrameFormats,
		r.compileCodecs,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return r.db, nil
}

func (r *resolver) resolveNodes() error {
	for _, raw := range r.raw.nodes {
		if _, exists := r.nodesByName[raw.name]; exists {
			return semanticError(raw.pos, "BU_", "duplicate node %q", raw.name)
		}
		r.nodesByName[raw.name] = len(r.db.Nodes)
		r.db.Nodes = append(r.db.Nodes, Node{Name: raw.name})
	}
	return nil
}

func (r *resolver) resolveMessages() error {
	r.signalsByMessage = make([]map[string]int, 0, len(r.raw.messages))
	r.messagePositions = make([]Position, 0, len(r.raw.messages))
	r.signalPositions = make([]map[string]Position, 0, len(r.raw.messages))
	r.simpleMuxValues = make([]map[string]uint64, 0, len(r.raw.messages))

	for _, raw := range r.raw.messages {
		if raw.id == vectorIndependentSignalsID || raw.name == "VECTOR__INDEPENDENT_SIG_MSG" {
			r.droppedMessages[raw.id] = true
			r.diag(raw.pos, "BO_", "independent-signal pseudo-message was ignored")
			continue
		}
		id, extended, err := normalizeID(raw.id)
		if err != nil {
			return semanticError(raw.pos, "BO_", "%v", err)
		}
		if _, exists := r.messagesByRawID[raw.id]; exists {
			return semanticError(raw.pos, "BO_", "duplicate message ID %d", raw.id)
		}
		if _, exists := r.messagesByName[raw.name]; exists {
			return semanticError(raw.pos, "BO_", "duplicate message name %q", raw.name)
		}

		message := Message{
			ID:           id,
			Extended:     extended,
			Name:         raw.name,
			Length:       raw.length,
			Transmitters: appendUnique(nil, raw.transmitter),
		}
		signalIndexes := make(map[string]int, len(raw.signals))
		signalPositions := make(map[string]Position, len(raw.signals))
		simpleMux := make(map[string]uint64)
		for _, rawSignal := range raw.signals {
			if _, exists := signalIndexes[rawSignal.name]; exists {
				return semanticError(rawSignal.pos, "SG_", "duplicate signal %q in message %q", rawSignal.name, raw.name)
			}
			isMultiplexer, muxValue, hasMuxValue, err := parseMuxMarker(rawSignal.muxMarker)
			if err != nil {
				return semanticError(rawSignal.pos, "SG_", "signal %q: %v", rawSignal.name, err)
			}
			signal := Signal{
				Name:          rawSignal.name,
				StartBit:      rawSignal.startBit,
				BitLength:     rawSignal.bitLength,
				ByteOrder:     rawSignal.byteOrder,
				Signed:        rawSignal.signed,
				ValueType:     ValueTypeInteger,
				Factor:        rawSignal.factor,
				Offset:        rawSignal.offset,
				Minimum:       rawSignal.minimum,
				Maximum:       rawSignal.maximum,
				Unit:          rawSignal.unit,
				Receivers:     append([]string(nil), rawSignal.receivers...),
				IsMultiplexer: isMultiplexer,
			}
			if err := validateSignalLayout(message.Length, signal); err != nil {
				return semanticError(rawSignal.pos, "SG_", "signal %q: %v", rawSignal.name, err)
			}
			if hasMuxValue {
				simpleMux[rawSignal.name] = muxValue
			}
			signalIndexes[rawSignal.name] = len(message.Signals)
			signalPositions[rawSignal.name] = rawSignal.pos
			message.Signals = append(message.Signals, signal)
		}

		messageIndex := len(r.db.Messages)
		r.messagesByRawID[raw.id] = messageIndex
		r.messagesByName[raw.name] = messageIndex
		r.db.Messages = append(r.db.Messages, message)
		r.signalsByMessage = append(r.signalsByMessage, signalIndexes)
		r.messagePositions = append(r.messagePositions, raw.pos)
		r.signalPositions = append(r.signalPositions, signalPositions)
		r.simpleMuxValues = append(r.simpleMuxValues, simpleMux)
	}
	return nil
}

func (r *resolver) resolveValueTables() error {
	seen := make(map[string]struct{}, len(r.raw.valueTables))
	for _, raw := range r.raw.valueTables {
		if _, exists := seen[raw.name]; exists {
			return semanticError(raw.pos, "VAL_TABLE_", "duplicate value table %q", raw.name)
		}
		seen[raw.name] = struct{}{}
		r.db.ValueTables = append(r.db.ValueTables, ValueTable{
			Name:   raw.name,
			Values: append([]ValueDescription(nil), raw.values...),
		})
	}
	return nil
}

func (r *resolver) resolveAttributeDefinitions() error {
	for _, raw := range r.raw.attributeDefs {
		if _, exists := r.definitions[raw.name]; exists {
			return semanticError(raw.pos, "BA_DEF_", "duplicate attribute definition %q", raw.name)
		}
		definition := AttributeDefinition{
			Name:    raw.name,
			Scope:   raw.scope,
			Kind:    raw.kind,
			Minimum: raw.minimum,
			Maximum: raw.maximum,
			Choices: append([]string(nil), raw.choices...),
		}
		r.definitions[raw.name] = len(r.db.AttributeDefinitions)
		r.db.AttributeDefinitions = append(r.db.AttributeDefinitions, definition)
	}

	for _, raw := range r.raw.attributeDefaults {
		index, exists := r.definitions[raw.name]
		if !exists {
			r.diag(raw.pos, "BA_DEF_DEF_", "default for undefined attribute %q was ignored", raw.name)
			continue
		}
		definition := &r.db.AttributeDefinitions[index]
		value, err := resolveLiteral(raw.value, *definition)
		if err != nil {
			return semanticError(raw.pos, "BA_DEF_DEF_", "attribute %q: %v", raw.name, err)
		}
		definition.Default = &value
	}
	return nil
}

func (r *resolver) resolveAttributeValues() error {
	for _, raw := range r.raw.attributeValues {
		definitionIndex, defined := r.definitions[raw.name]
		var value AttributeValue
		var err error
		if defined {
			definition := r.db.AttributeDefinitions[definitionIndex]
			if definition.Scope != raw.scope {
				return semanticError(raw.pos, "BA_", "attribute %q targets %s but is defined for %s", raw.name, scopeName(raw.scope), scopeName(definition.Scope))
			}
			value, err = resolveLiteral(raw.value, definition)
		} else {
			value, err = inferLiteral(raw.value)
			r.diag(raw.pos, "BA_", "attribute %q has no definition; its value type was inferred", raw.name)
		}
		if err != nil {
			return semanticError(raw.pos, "BA_", "attribute %q: %v", raw.name, err)
		}

		target, ok := r.attributeTarget(raw)
		if !ok {
			continue
		}
		if *target == nil {
			*target = make(map[string]AttributeValue)
		}
		if _, exists := (*target)[raw.name]; exists {
			return semanticError(raw.pos, "BA_", "duplicate assignment of attribute %q", raw.name)
		}
		(*target)[raw.name] = value
	}
	return nil
}

func (r *resolver) resolveComments() error {
	for _, raw := range r.raw.comments {
		switch raw.scope {
		case AttributeScopeDatabase:
			r.db.Comment = raw.text
		case AttributeScopeNode:
			index, exists := r.nodesByName[raw.object]
			if !exists {
				r.diag(raw.pos, "CM_", "comment for unknown node %q was ignored", raw.object)
				continue
			}
			r.db.Nodes[index].Comment = raw.text
		case AttributeScopeMessage:
			message, _, ok := r.message(raw.messageID, raw.pos, "CM_")
			if !ok {
				continue
			}
			message.Comment = raw.text
		case AttributeScopeSignal:
			_, signal, ok := r.signal(raw.messageID, raw.object, raw.pos, "CM_")
			if !ok {
				continue
			}
			signal.Comment = raw.text
		}
	}
	return nil
}

func (r *resolver) resolveTransmitters() error {
	for _, raw := range r.raw.transmitters {
		message, _, ok := r.message(raw.messageID, raw.pos, "BO_TX_BU_")
		if !ok {
			continue
		}
		for _, transmitter := range raw.transmitters {
			message.Transmitters = appendUnique(message.Transmitters, transmitter)
		}
	}
	return nil
}

func (r *resolver) resolveValueLists() error {
	seen := make(map[string]struct{}, len(r.raw.valueLists))
	for _, raw := range r.raw.valueLists {
		_, signal, ok := r.signal(raw.messageID, raw.signal, raw.pos, "VAL_")
		if !ok {
			continue
		}
		key := fmt.Sprintf("%d:%s", raw.messageID, raw.signal)
		if _, exists := seen[key]; exists {
			return semanticError(raw.pos, "VAL_", "duplicate value descriptions for signal %q", raw.signal)
		}
		seen[key] = struct{}{}
		signal.Values = append([]ValueDescription(nil), raw.values...)
	}
	return nil
}

func (r *resolver) resolveValueTypes() error {
	seen := make(map[string]struct{}, len(r.raw.valueTypes))
	for _, raw := range r.raw.valueTypes {
		_, signal, ok := r.signal(raw.messageID, raw.signal, raw.pos, "SIG_VALTYPE_")
		if !ok {
			continue
		}
		key := fmt.Sprintf("%d:%s", raw.messageID, raw.signal)
		if _, exists := seen[key]; exists {
			return semanticError(raw.pos, "SIG_VALTYPE_", "duplicate value type for signal %q", raw.signal)
		}
		seen[key] = struct{}{}
		if raw.valueType == ValueTypeFloat32 && signal.BitLength != 32 {
			return semanticError(raw.pos, "SIG_VALTYPE_", "float32 signal %q must be 32 bits", raw.signal)
		}
		if raw.valueType == ValueTypeFloat64 && signal.BitLength != 64 {
			return semanticError(raw.pos, "SIG_VALTYPE_", "float64 signal %q must be 64 bits", raw.signal)
		}
		signal.ValueType = raw.valueType
	}
	return nil
}

func (r *resolver) resolveSignalGroups() error {
	for _, raw := range r.raw.signalGroups {
		message, messageIndex, ok := r.message(raw.messageID, raw.pos, "SIG_GROUP_")
		if !ok {
			continue
		}
		for _, existing := range message.SignalGroups {
			if existing.Name == raw.name {
				return semanticError(raw.pos, "SIG_GROUP_", "duplicate signal group %q", raw.name)
			}
		}
		resolved := true
		for _, name := range raw.signals {
			if _, exists := r.signalsByMessage[messageIndex][name]; !exists {
				r.diag(raw.pos, "SIG_GROUP_", "group %q with unknown signal %q was ignored", raw.name, name)
				resolved = false
				break
			}
		}
		if !resolved {
			continue
		}
		message.SignalGroups = append(message.SignalGroups, SignalGroup{
			Name: raw.name, Repetitions: raw.repetitions, Signals: append([]string(nil), raw.signals...),
		})
	}
	return nil
}

func (r *resolver) resolveMultiplexing() error {
	for _, raw := range r.raw.multiplex {
		_, signal, ok := r.signal(raw.messageID, raw.signal, raw.pos, "SG_MUL_VAL_")
		if !ok {
			continue
		}
		_, selector, ok := r.signal(raw.messageID, raw.selector, raw.pos, "SG_MUL_VAL_")
		if !ok {
			continue
		}
		if !selector.IsMultiplexer {
			return semanticError(raw.pos, "SG_MUL_VAL_", "selector %q is not declared as a multiplexer", raw.selector)
		}
		if signal.Name == selector.Name {
			return semanticError(raw.pos, "SG_MUL_VAL_", "signal %q cannot multiplex itself", raw.signal)
		}
		if signal.Multiplex != nil && signal.Multiplex.Selector != raw.selector {
			return semanticError(raw.pos, "SG_MUL_VAL_", "signal %q has more than one selector", raw.signal)
		}
		if signal.Multiplex == nil {
			signal.Multiplex = &MultiplexCondition{Selector: raw.selector}
		}
		signal.Multiplex.Ranges = append(signal.Multiplex.Ranges, raw.ranges...)
	}

	for messageIndex := range r.db.Messages {
		message := &r.db.Messages[messageIndex]
		var roots []string
		for index := range message.Signals {
			signal := &message.Signals[index]
			_, hasSimpleValue := r.simpleMuxValues[messageIndex][signal.Name]
			if signal.IsMultiplexer && signal.Multiplex == nil && !hasSimpleValue {
				roots = append(roots, signal.Name)
			}
		}
		for index := range message.Signals {
			signal := &message.Signals[index]
			value, hasSimpleValue := r.simpleMuxValues[messageIndex][signal.Name]
			if !hasSimpleValue {
				continue
			}
			if signal.Multiplex != nil {
				if !rangesContain(signal.Multiplex.Ranges, value) {
					return semanticError(r.signalPositions[messageIndex][signal.Name], "SG_MUL_VAL_", "signal %q simple multiplex value %d is outside its extended ranges", signal.Name, value)
				}
				continue
			}
			if len(roots) != 1 {
				return semanticError(r.signalPositions[messageIndex][signal.Name], "SG_", "signal %q needs SG_MUL_VAL_ because message %q has %d root multiplexers", signal.Name, message.Name, len(roots))
			}
			signal.Multiplex = &MultiplexCondition{
				Selector: roots[0],
				Ranges:   []MultiplexRange{{First: value, Last: value}},
			}
		}
		if err := validateMultiplexGraph(*message); err != nil {
			return semanticError(r.messagePositions[messageIndex], "SG_MUL_VAL_", "message %q: %v", message.Name, err)
		}
	}
	return nil
}

// resolveFrameFormats stamps each message's frame format from VFrameFormat,
// the database ProtocolType, or the identifier and length. The format guards
// the length and identifier validation; further attribute interpretation
// belongs to the layers above.
func (r *resolver) resolveFrameFormats() error {
	protocol := effectiveAttribute(r.db.Attributes, r.definition("ProtocolType"))
	for index := range r.db.Messages {
		message := &r.db.Messages[index]
		format := effectiveAttribute(message.Attributes, r.definition("VFrameFormat"))
		if format != nil {
			resolved, err := frameFormat(*format)
			if err != nil {
				return semanticError(r.messagePositions[index], "BA_", "message %q: %v", message.Name, err)
			}
			message.Format = resolved
		} else {
			if protocol != nil && strings.EqualFold(protocol.Text, "J1939") {
				message.Format = FrameFormatJ1939
			} else if message.Length <= 8 {
				if message.Extended {
					message.Format = FrameFormatExtendedCAN
				} else {
					message.Format = FrameFormatStandardCAN
				}
			}
		}
		if err := validateFrameFormat(*message); err != nil {
			return semanticError(r.messagePositions[index], "BA_", "message %q: %v", message.Name, err)
		}
		brs := effectiveAttribute(message.Attributes, r.definition("CANFD_BRS"))
		if brs != nil {
			switch {
			case brs.Integer == 0 && (brs.Text == "" || brs.Text == "0"):
				message.bitRateSwitch = false
			case brs.Integer == 1 || brs.Text == "1":
				message.bitRateSwitch = true
			default:
				return semanticError(r.messagePositions[index], "BA_", "message %q: unsupported CANFD_BRS value %q (%d)", message.Name, brs.Text, brs.Integer)
			}
		}
	}
	return nil
}

func (r *resolver) compileCodecs() error {
	r.db.messagesByName = make(map[string]int, len(r.messagesByName))
	for name, index := range r.messagesByName {
		r.db.messagesByName[name] = index
	}
	for index := range r.db.Messages {
		r.db.Messages[index].codec = compileMessageCodec(&r.db.Messages[index])
	}
	return nil
}

func (r *resolver) attributeTarget(raw rawAttributeAssignment) (*map[string]AttributeValue, bool) {
	switch raw.scope {
	case AttributeScopeNode:
		index, exists := r.nodesByName[raw.object]
		if !exists {
			r.diag(raw.pos, "BA_", "attribute for unknown node %q was ignored", raw.object)
			return nil, false
		}
		return &r.db.Nodes[index].Attributes, true
	case AttributeScopeMessage:
		message, _, ok := r.message(raw.messageID, raw.pos, "BA_")
		if !ok {
			return nil, false
		}
		return &message.Attributes, true
	case AttributeScopeSignal:
		_, signal, ok := r.signal(raw.messageID, raw.object, raw.pos, "BA_")
		if !ok {
			return nil, false
		}
		return &signal.Attributes, true
	default:
		return &r.db.Attributes, true
	}
}

// message resolves a message reference. A dangling reference reports a
// diagnostic and returns false so the caller can drop its record; references
// to dropped pseudo-messages are skipped silently.
func (r *resolver) message(rawID uint32, pos Position, keyword string) (*Message, int, bool) {
	index, exists := r.messagesByRawID[rawID]
	if !exists {
		if !r.droppedMessages[rawID] {
			r.diag(pos, keyword, "record for unknown message ID %d was ignored", rawID)
		}
		return nil, 0, false
	}
	return &r.db.Messages[index], index, true
}

func (r *resolver) signal(rawID uint32, name string, pos Position, keyword string) (*Message, *Signal, bool) {
	message, messageIndex, ok := r.message(rawID, pos, keyword)
	if !ok {
		return nil, nil, false
	}
	signalIndex, exists := r.signalsByMessage[messageIndex][name]
	if !exists {
		r.diag(pos, keyword, "record for unknown signal %q in message %q was ignored", name, message.Name)
		return nil, nil, false
	}
	return message, &message.Signals[signalIndex], true
}

func (r *resolver) diag(pos Position, keyword, format string, args ...any) {
	r.db.Diagnostics = append(r.db.Diagnostics, Diagnostic{
		Position: pos, Keyword: keyword, Message: fmt.Sprintf(format, args...),
	})
}

func (r *resolver) definition(name string) *AttributeDefinition {
	index, exists := r.definitions[name]
	if !exists {
		return nil
	}
	return &r.db.AttributeDefinitions[index]
}

func normalizeID(raw uint32) (uint32, bool, error) {
	if raw&0x80000000 != 0 {
		if raw&0x60000000 != 0 {
			return 0, false, fmt.Errorf("extended identifier %d has bits outside the 29-bit CAN ID", raw)
		}
		return raw & 0x1fffffff, true, nil
	}
	if raw > 0x7ff {
		return 0, false, fmt.Errorf("standard identifier %#x exceeds 11 bits; extended DBC IDs must set bit 31", raw)
	}
	return raw, false, nil
}

func parseMuxMarker(marker string) (isMultiplexer bool, value uint64, hasValue bool, err error) {
	if marker == "" {
		return false, 0, false, nil
	}
	if marker == "M" {
		return true, 0, false, nil
	}
	if !strings.HasPrefix(marker, "m") {
		return false, 0, false, fmt.Errorf("invalid multiplex marker %q", marker)
	}
	valueText := strings.TrimPrefix(marker, "m")
	if strings.HasSuffix(valueText, "M") {
		isMultiplexer = true
		valueText = strings.TrimSuffix(valueText, "M")
	}
	if valueText == "" {
		return false, 0, false, fmt.Errorf("invalid multiplex marker %q", marker)
	}
	value, parseErr := strconv.ParseUint(valueText, 10, 64)
	if parseErr != nil {
		return false, 0, false, fmt.Errorf("invalid multiplex marker %q", marker)
	}
	return isMultiplexer, value, true, nil
}

func validateSignalLayout(messageLength uint32, signal Signal) error {
	// Only layout bounds are validated here. Overlap between signals is
	// conditional on multiplexing, so the message codec rejects it on the
	// write path while parsing stays permissive for read-only databases.
	if signal.BitLength == 0 {
		return fmt.Errorf("bit length is zero")
	}
	messageBits := uint64(messageLength) * 8
	if signal.ByteOrder == ByteOrderLittleEndian {
		end := uint64(signal.StartBit) + uint64(signal.BitLength)
		if end > messageBits {
			return fmt.Errorf("bits %d..%d exceed %d-byte message", signal.StartBit, end-1, messageLength)
		}
		return nil
	}

	bit := uint64(signal.StartBit)
	for index := uint32(0); index < signal.BitLength; index++ {
		if bit >= messageBits {
			return fmt.Errorf("big-endian bit walk exceeds %d-byte message", messageLength)
		}
		if index+1 == signal.BitLength {
			break
		}
		if bit%8 == 0 {
			bit += 15
		} else {
			bit--
		}
	}
	return nil
}

func validateMultiplexGraph(message Message) error {
	selectors := make(map[string]string)
	for _, signal := range message.Signals {
		if signal.Multiplex != nil {
			selectors[signal.Name] = signal.Multiplex.Selector
		}
	}
	for start := range selectors {
		seen := make(map[string]struct{})
		for current := start; current != ""; current = selectors[current] {
			if _, exists := seen[current]; exists {
				return fmt.Errorf("multiplexer cycle includes signal %q", current)
			}
			seen[current] = struct{}{}
		}
	}
	return nil
}

func rangesContain(ranges []MultiplexRange, value uint64) bool {
	for _, item := range ranges {
		if item.First <= value && value <= item.Last {
			return true
		}
	}
	return false
}

func resolveLiteral(raw rawLiteral, definition AttributeDefinition) (AttributeValue, error) {
	value := AttributeValue{Kind: definition.Kind}
	switch definition.Kind {
	case AttributeKindString:
		if !raw.isString {
			return AttributeValue{}, fmt.Errorf("expected a quoted string")
		}
		value.Text = raw.text
	case AttributeKindEnum:
		if raw.isString {
			value.Integer = -1
			value.Text = raw.text
			for index, choice := range definition.Choices {
				if choice == raw.text {
					value.Integer = int64(index)
					break
				}
			}
			return value, nil
		}
		index, err := strconv.ParseInt(raw.text, 0, 64)
		if err != nil {
			return AttributeValue{}, fmt.Errorf("invalid enum index %q", raw.text)
		}
		value.Integer = index
		if index >= 0 && index < int64(len(definition.Choices)) {
			value.Text = definition.Choices[index]
		}
	case AttributeKindInteger, AttributeKindHex:
		if raw.isString {
			return AttributeValue{}, fmt.Errorf("expected an integer")
		}
		integer, err := strconv.ParseInt(raw.text, 0, 64)
		if err != nil {
			return AttributeValue{}, fmt.Errorf("invalid integer %q", raw.text)
		}
		value.Integer = integer
	case AttributeKindFloat:
		if raw.isString {
			return AttributeValue{}, fmt.Errorf("expected a number")
		}
		floating, err := strconv.ParseFloat(raw.text, 64)
		if err != nil {
			return AttributeValue{}, fmt.Errorf("invalid number %q", raw.text)
		}
		value.Float = floating
	default:
		return AttributeValue{}, fmt.Errorf("unknown attribute kind")
	}
	return value, nil
}

func inferLiteral(raw rawLiteral) (AttributeValue, error) {
	if raw.isString {
		return AttributeValue{Kind: AttributeKindString, Text: raw.text}, nil
	}
	if strings.ContainsAny(raw.text, ".eE") {
		value, err := strconv.ParseFloat(raw.text, 64)
		if err != nil {
			return AttributeValue{}, fmt.Errorf("invalid number %q", raw.text)
		}
		return AttributeValue{Kind: AttributeKindFloat, Float: value}, nil
	}
	value, err := strconv.ParseInt(raw.text, 0, 64)
	if err != nil {
		return AttributeValue{}, fmt.Errorf("invalid integer %q", raw.text)
	}
	return AttributeValue{Kind: AttributeKindInteger, Integer: value}, nil
}

func effectiveAttribute(values map[string]AttributeValue, definition *AttributeDefinition) *AttributeValue {
	if definition == nil {
		return nil
	}
	if value, exists := values[definition.Name]; exists {
		return &value
	}
	return definition.Default
}

func frameFormat(value AttributeValue) (FrameFormat, error) {
	switch value.Text {
	case "StandardCAN":
		return FrameFormatStandardCAN, nil
	case "ExtendedCAN":
		return FrameFormatExtendedCAN, nil
	case "StandardCAN_FD":
		return FrameFormatStandardCANFD, nil
	case "ExtendedCAN_FD":
		return FrameFormatExtendedCANFD, nil
	case "J1939PG":
		return FrameFormatJ1939, nil
	}
	switch value.Integer {
	case 0:
		return FrameFormatStandardCAN, nil
	case 1:
		return FrameFormatExtendedCAN, nil
	case 3:
		return FrameFormatJ1939, nil
	case 14:
		return FrameFormatStandardCANFD, nil
	case 15:
		return FrameFormatExtendedCANFD, nil
	default:
		return FrameFormatUnknown, fmt.Errorf("unsupported VFrameFormat %q (%d)", value.Text, value.Integer)
	}
}

func validateFrameFormat(message Message) error {
	if message.Length > 64 {
		return fmt.Errorf("CAN message length %d exceeds 64", message.Length)
	}
	switch message.Format {
	case FrameFormatStandardCAN, FrameFormatStandardCANFD:
		if message.Extended {
			return fmt.Errorf("standard frame format conflicts with extended identifier")
		}
	case FrameFormatExtendedCAN, FrameFormatExtendedCANFD, FrameFormatJ1939:
		if !message.Extended {
			return fmt.Errorf("extended frame format requires an extended identifier")
		}
	}
	if (message.Format == FrameFormatStandardCAN || message.Format == FrameFormatExtendedCAN) && message.Length > 8 {
		return fmt.Errorf("classical CAN message length %d exceeds 8", message.Length)
	}
	if (message.Format == FrameFormatStandardCANFD || message.Format == FrameFormatExtendedCANFD) && message.Length > 64 {
		return fmt.Errorf("CAN FD message length %d exceeds 64", message.Length)
	}
	return nil
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func scopeName(scope AttributeScope) string {
	switch scope {
	case AttributeScopeDatabase:
		return "database"
	case AttributeScopeNode:
		return "node"
	case AttributeScopeMessage:
		return "message"
	case AttributeScopeSignal:
		return "signal"
	default:
		return "unknown"
	}
}

func semanticError(pos Position, keyword, format string, args ...any) error {
	return &Error{Position: pos, Keyword: keyword, Message: fmt.Sprintf(format, args...)}
}
