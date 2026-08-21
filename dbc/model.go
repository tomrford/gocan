// Package dbc parses CAN database files into a resolved semantic model and
// encodes or lazily decodes raw CAN frames.
package dbc

import (
	"github.com/tomrford/gocan/internal/scalar"
	"github.com/tomrford/gocan/j1939"
)

// Database is one resolved DBC file.
//
// Slices retain declaration order. Maps contain explicit attributes; defaults
// remain on their definitions so callers can distinguish the two.
type Database struct {
	Version              string
	Nodes                []Node
	Messages             []Message
	ValueTables          []ValueTable
	AttributeDefinitions []AttributeDefinition
	Attributes           map[string]AttributeValue
	Diagnostics          []Diagnostic

	messagesByName map[string]int
	messagesByPGN  map[j1939.PGN][]int
}

// Node is one CAN network participant declared by BU_.
type Node struct {
	Name       string
	Attributes map[string]AttributeValue
}

// Message is one resolved BO_ definition.
type Message struct {
	ID           uint32
	Extended     bool
	Name         string
	Length       uint32
	Transmitters []string
	Signals      []Signal
	SignalGroups []SignalGroup
	Format       FrameFormat
	Attributes   map[string]AttributeValue

	bitRateSwitch bool
	codec         *messageCodec
}

// Values maps runtime-loaded signal names to their values. Encoding accepts
// Go numeric values, bool for one-bit signals, and value-description strings.
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
type Signal struct {
	Name          string
	StartBit      uint32
	BitLength     uint32
	ByteOrder     ByteOrder
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
}

// ByteOrder is the DBC signal byte order.
type ByteOrder uint8

const (
	ByteOrderBigEndian ByteOrder = iota
	ByteOrderLittleEndian
)

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

// ValueDescription assigns a label to one raw integer value. It shares its
// identity with the CDD choice type, so label metadata moves between the two
// catalogs without conversion.
type ValueDescription = scalar.Choice

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
// objects, and the Vector independent-signal pseudo-message.
type Diagnostic struct {
	Position Position
	Keyword  string
	Message  string
}
