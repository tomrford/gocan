// Package cdd parses the supported CANdela diagnostic description subset into
// a resolved data-identifier catalog.
package cdd

// Database is the resolved catalog from the first ECU and its first variant in
// a CDD document. DIDs remain in source order.
type Database struct {
	DIDs []DID

	didsByName       map[string]int
	didsByIdentifier map[uint16]int
}

// DID describes one UDS data identifier and its fixed payload layout.
type DID struct {
	Name       string
	Identifier uint16
	Length     uint32
	Fields     []Field
}

// ByteOrder describes the byte order of one coded field.
type ByteOrder uint8

const (
	// ByteOrderBig stores the most significant byte first.
	ByteOrderBig ByteOrder = iota + 1
	// ByteOrderLittle stores the least significant byte first.
	ByteOrderLittle
)

// Encoding describes the coded representation declared by CVALUETYPE.
type Encoding string

const (
	EncodingUnsigned Encoding = "uns"
	EncodingSigned   Encoding = "sgn"
	EncodingASCII    Encoding = "asc"
	EncodingUTF      Encoding = "utf"
	EncodingBCD      Encoding = "bcd"
	EncodingFloat    Encoding = "dbl"
)

// LinearConversion maps a coded numeric value to its physical value.
type LinearConversion struct {
	Scale  float64
	Offset float64
}

// Choice assigns a label to one exact coded integer value.
type Choice struct {
	Value int64
	Label string
}

// Field describes one fixed-size coded field. BitOffset is a linear offset
// from the start of the DID data record; it does not use DBC bit numbering.
type Field struct {
	Name       string
	BitOffset  uint32
	BitLength  uint32
	ByteOrder  ByteOrder
	Encoding   Encoding
	Conversion *LinearConversion
	Unit       string
	Choices    []Choice
}

// DIDByName returns the DID with name.
func (database *Database) DIDByName(name string) (*DID, bool) {
	if database == nil {
		return nil, false
	}
	index, ok := database.didsByName[name]
	if !ok {
		return nil, false
	}
	return &database.DIDs[index], true
}

// DIDByIdentifier returns the DID with identifier.
func (database *Database) DIDByIdentifier(identifier uint16) (*DID, bool) {
	if database == nil {
		return nil, false
	}
	index, ok := database.didsByIdentifier[identifier]
	if !ok {
		return nil, false
	}
	return &database.DIDs[index], true
}
