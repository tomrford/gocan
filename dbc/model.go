// Package dbc parses CAN database files into a resolved semantic model,
// writes that model back as DBC text, and encodes or lazily decodes raw CAN
// frames.
package dbc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"maps"
	"slices"
)

// Database is one resolved DBC file.
//
// Slices retain declaration order. Maps contain explicit attributes; defaults
// remain on their definitions so callers can distinguish the two.
// Treat the database and its nested models as read-only: lookups and codecs
// trust the model that Parse resolved, and modifications are not revalidated.
// Use Edit to derive a changed database.
type Database struct {
	Version              string
	Nodes                []Node
	Messages             []Message
	ValueTables          []ValueTable
	AttributeDefinitions []AttributeDefinition
	Attributes           map[string]AttributeValue
	Comment              string
	Diagnostics          []Diagnostic

	messagesByName map[string]int
	messagesByID   map[messageID]int
	messagesByPGN  map[uint32][]int
	source         []byte
	encoding       sourceEncoding
}

type sourceEncoding uint8

const (
	sourceEncodingUTF8 sourceEncoding = iota
	sourceEncodingUTF8BOM
	sourceEncodingWindows1252
)

// messageID indexes one message by its resolved arbitration identifier. The
// extended flag is part of the identity: a standard and an extended message may
// share the same numeric identifier.
type messageID struct {
	id       uint32
	extended bool
}

// clone returns a deep copy of db's exported model. It clears cached lookup
// maps and per-message codecs so editing messages or signals on the copy never
// indexes stale positions; lookups scan the copy's live slices, and codec
// methods reject the copy until Edit reparses it.
//
// clone rejects a signal byte order other than binary.BigEndian or
// binary.LittleEndian, which Write cannot express and which may share state.
func (db *Database) clone() (*Database, error) {
	clone := *db
	clone.messagesByName = nil
	clone.messagesByID = nil
	clone.messagesByPGN = nil
	clone.Nodes = slices.Clone(db.Nodes)
	for index := range clone.Nodes {
		clone.Nodes[index].Attributes = maps.Clone(clone.Nodes[index].Attributes)
	}
	clone.Messages = slices.Clone(db.Messages)
	for index := range clone.Messages {
		message := &clone.Messages[index]
		message.codec = nil
		message.Transmitters = slices.Clone(message.Transmitters)
		message.Signals = slices.Clone(message.Signals)
		for index := range message.Signals {
			signal := &message.Signals[index]
			if signal.ByteOrder != binary.BigEndian && signal.ByteOrder != binary.LittleEndian {
				return nil, fmt.Errorf("%w: signal %q has unsupported byte order %v", ErrUnrepresentable, signal.Name, signal.ByteOrder)
			}
			signal.Receivers = slices.Clone(signal.Receivers)
			signal.Values = slices.Clone(signal.Values)
			if signal.Multiplex != nil {
				multiplex := *signal.Multiplex
				multiplex.Ranges = slices.Clone(multiplex.Ranges)
				signal.Multiplex = &multiplex
			}
			signal.Attributes = maps.Clone(signal.Attributes)
		}
		message.SignalGroups = slices.Clone(message.SignalGroups)
		for index := range message.SignalGroups {
			message.SignalGroups[index].Signals = slices.Clone(message.SignalGroups[index].Signals)
		}
		message.Attributes = maps.Clone(message.Attributes)
	}
	clone.ValueTables = slices.Clone(db.ValueTables)
	for index := range clone.ValueTables {
		clone.ValueTables[index].Values = slices.Clone(clone.ValueTables[index].Values)
	}
	clone.AttributeDefinitions = slices.Clone(db.AttributeDefinitions)
	for index := range clone.AttributeDefinitions {
		definition := &clone.AttributeDefinitions[index]
		definition.Choices = slices.Clone(definition.Choices)
		if definition.Default != nil {
			value := *definition.Default
			definition.Default = &value
		}
	}
	clone.Attributes = maps.Clone(db.Attributes)
	clone.Diagnostics = slices.Clone(db.Diagnostics)
	return &clone, nil
}

// Source returns a copy of the exact bytes read by Parse or ParseFile, or
// written by Edit, including its original encoding and byte-order mark. It
// does not reflect in-place model edits. A database constructed without
// parsing or editing returns nil.
func (db *Database) Source() []byte { return bytes.Clone(db.source) }

// Node is one CAN network participant declared by BU_.
type Node struct {
	Name       string
	Attributes map[string]AttributeValue
	Comment    string
}

// Message is one resolved BO_ definition.
// Treat it and its nested fields as read-only, as described by Database.
type Message struct {
	ID           uint32
	Extended     bool
	Name         string
	Length       uint32
	Transmitters []string
	Signals      []Signal
	SignalGroups []SignalGroup
	// Format is derived from VFrameFormat and ProtocolType attributes and the
	// message's identifier and length. Edit those declarations to change it;
	// Write does not serialize Format itself.
	Format     FrameFormat
	Attributes map[string]AttributeValue
	Comment    string

	bitRateSwitch bool
	codec         *messageCodec
}

// Values maps runtime-loaded signal names to their values. Encoding accepts
// Go numeric values, json.Number, bool for one-bit signals, and value-description
// strings. Unscaled integer signals require exact integers. Floating-point
// signals use normal rounding, with float64 arithmetic when scaled.
// Scaled integer signals use float64 arithmetic and round to the nearest raw
// value; native Go integers must first convert to float64 exactly, while JSON
// numbers use normal float64 rounding.
type Values map[string]any

// FrameFormat is the message format declared by VFrameFormat.
type FrameFormat uint8

const (
	FrameFormatUnknown FrameFormat = iota
	FrameFormatStandardCAN
	FrameFormatExtendedCAN
	FrameFormatStandardCANFD
	FrameFormatExtendedCANFD
	FrameFormatJ1939
)

// Signal is one resolved SG_ definition.
// ByteOrder is binary.BigEndian (@0, Motorola) or binary.LittleEndian (@1, Intel).
type Signal struct {
	Name          string
	StartBit      uint32
	BitLength     uint32
	ByteOrder     binary.ByteOrder
	Signed        bool
	ValueType     ValueType
	Factor        float64
	Offset        float64
	Minimum       float64
	Maximum       float64
	Unit          string
	Receivers     []string
	Values        []ValueDescription
	IsMultiplexer bool
	Multiplex     *MultiplexCondition
	Attributes    map[string]AttributeValue
	Comment       string
}

// ValueType is the raw numeric representation of a signal.
type ValueType uint8

const (
	ValueTypeInteger ValueType = iota
	ValueTypeFloat32
	ValueTypeFloat64
)

// MultiplexCondition states when a signal is present.
type MultiplexCondition struct {
	Selector string
	Ranges   []MultiplexRange
}

// MultiplexRange is one inclusive range of selector values.
type MultiplexRange struct {
	First uint64
	Last  uint64
}

// ValueDescription assigns a label to one raw integer value.
type ValueDescription struct {
	Value int64
	Label string
}

// ValueTable is one global VAL_TABLE_ declaration.
type ValueTable struct {
	Name   string
	Values []ValueDescription
}

// SignalGroup is one SIG_GROUP_ declaration.
type SignalGroup struct {
	Name        string
	Repetitions uint32
	Signals     []string
}

// AttributeScope identifies the object type targeted by an attribute.
type AttributeScope uint8

const (
	AttributeScopeDatabase AttributeScope = iota
	AttributeScopeNode
	AttributeScopeMessage
	AttributeScopeSignal
)

// AttributeKind is the value type declared by BA_DEF_.
type AttributeKind uint8

const (
	AttributeKindInteger AttributeKind = iota
	AttributeKindHex
	AttributeKindFloat
	AttributeKindString
	AttributeKindEnum
)

// AttributeDefinition describes one BA_DEF_ and its optional BA_DEF_DEF_.
type AttributeDefinition struct {
	Name    string
	Scope   AttributeScope
	Kind    AttributeKind
	Minimum float64
	Maximum float64
	Choices []string
	Default *AttributeValue
}

// AttributeValue is one resolved attribute value.
//
// Integer is used by integer, hexadecimal, and enum attributes. Text contains
// string values or the enum label. An enum label not present in its definition
// has an Integer value of -1; an unknown numeric enum index has empty Text.
type AttributeValue struct {
	Kind    AttributeKind
	Integer int64
	Float   float64
	Text    string
}

// Position identifies a location in the parsed source.
type Position struct {
	Source string
	Line   int
	Column int
}

// Diagnostic reports a record that was dropped without making the database
// unsafe to use: unsupported record types, dangling references to unknown
// objects, the Vector independent-signal pseudo-message, and comments replaced
// by a later comment for the same object.
type Diagnostic struct {
	Position Position
	Keyword  string
	Message  string
}
