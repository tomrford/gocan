// Package cdd parses the supported CANdela diagnostic description subset into
// a resolved data-identifier catalog and encodes or decodes DID data records.
//
// The record codec supports byte-aligned integer elements up to 64 bits,
// 32- and 64-bit floating-point elements, 8-bit ASCII, fixed arrays, and a
// variable array in final position. Parsed fields outside this set remain in
// the model, but Record.Encode and Record.Decode report an error.
package cdd

import "github.com/tomrford/gocan/internal/scalar"

// Database is the resolved catalog from the first ECU and its first variant in
// a CDD document. DIDs remain in source order.
//
// States retains every declared state in document order. Sessions and
// SecurityLevels contain the states of the corresponding CDD groups, including
// any locked state. These are literal document metadata, not UDS subfunctions
// or a hierarchy of access privileges.
type Database struct {
	DIDs           []DID
	States         []State
	Sessions       []State
	SecurityLevels []State
	Diagnostics    []Diagnostic

	didsByName       map[string]int
	didsByIdentifier map[uint16]int
}

// Diagnostic reports a defect confined to one data identifier that leaves the
// database safe to use. A DID is dropped from the catalog when its layout is
// outside the supported subset, its references do not resolve, or its name or
// identifier repeats. Unresolved execution preconditions leave the DID usable
// for encoding and decoding, with an error on the affected Precondition. Name is
// the QUAL of the diagnostic instance, which CDD documents may leave empty.
type Diagnostic struct {
	Name    string
	Message string
}

// DID describes one UDS data identifier. Read and Write are nil when the CDD
// does not define that operation for the identifier.
type DID struct {
	Name       string
	Identifier uint16
	Read       *Record
	Write      *Record
}

// Record describes the payload layout for one DID operation. Fields are in
// layout order.
//
// Length is the smallest data-record size in bytes. MaxLength exceeds it only
// when the last field is variable length, in which case MaxLength is the
// largest record.
//
// Preconditions preserves alternative service conditions in source order.
// There is one entry per source service bound to this supported record
// operation, even when conditions are equivalent.
// Conditions from different alternatives must not be combined. An unresolved
// alternative has Err set; it does not prevent encoding or decoding the record.
//
// Treat a Record and its Fields as read-only: Encode and Decode trust the
// layout that Parse resolved, and modifications are not revalidated.
type Record struct {
	Name          string
	Length        uint32
	MaxLength     uint32
	Fields        []Field
	Preconditions []Precondition

	codec *recordCodec
}

// SourceIdentity retains the XML id, oid, temploid and QUAL of a CDD object.
// Missing attributes remain empty; qualifiers need not be unique. These values
// identify document objects and must not be used as UDS subfunctions.
type SourceIdentity struct {
	ID          string
	OID         string
	TemplateOID string
	Qualifier   string
}

// State is a literal CDD state and its containing group. Index is its one-based
// position across all STATEGROUPS/STATE entries, including unsupported groups.
// GroupIndex is the one-based position of its STATEGROUP. Index describes
// source order only; its relationship to mayBeExec values is not established.
// Neither index is a UDS subfunction. No initial state is inferred.
type State struct {
	Source     SourceIdentity
	Index      int
	Group      SourceIdentity
	GroupIndex int
	GroupSpec  string
}

// Precondition describes one source service's execution requirements.
// ServiceIndex is its one-based position among all SERVICE children of the
// diagnostic instance, including services outside the supported DID subset.
// TemplateRef is the instance's literal tmplref.
//
// Check Err first: a non-nil error means unknown requirements, not ECU rejection.
// A nil Err means no explicit CDD restriction was found on the service or its
// template. This does not establish ECU permission. Callers manage ECU state.
//
// Raw rule pointers distinguish missing attributes from explicit empty values.
// Parse currently leaves every explicit rule unresolved because the CDD index,
// omitted-group, exclusion and inheritance rules have not been verified.
// Resolved restrictions are not exposed until those rules are established.
type Precondition struct {
	Service                      SourceIdentity
	ServiceIndex                 int
	TemplateRef                  string
	MayBeExec                    *string
	NotExecInStateGroups         *string
	TemplateMayBeExec            *string
	TemplateNotExecInStateGroups *string
	Err                          error
}

// Values maps DID field names to their physical values. Encoding accepts Go
// numeric values, strings for ASCII fields, and choice labels.
type Values map[string]any

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
	EncodingFloat    Encoding = "flt"
	EncodingDouble   Encoding = "dbl"
)

// LinearConversion maps a coded numeric value to its physical value.
type LinearConversion struct {
	Scale  float64
	Offset float64
}

// Choice assigns a label to one exact coded integer value. It shares its
// identity with the DBC value-description type, so label metadata moves
// between the two catalogs without conversion.
type Choice = scalar.Choice

// Field describes one fixed-size coded field. BitOffset is a linear offset
// from the start of the DID data record; it does not use DBC bit numbering.
//
// BitLength is the width of a single element and Count its repetition, so the
// field occupies BitLength*Count bits. Count is 1 for scalars and greater for
// fixed-size arrays such as ASCII serial numbers and calibration blocks.
//
// Variable is set when the element count is only known from the length of the
// response. Count then repeats Variable.MinCount, so BitSize remains the number
// of bits the field always occupies.
type Field struct {
	Name       string
	BitOffset  uint32
	BitLength  uint32
	Count      uint32
	Variable   *Extent
	ByteOrder  ByteOrder
	Encoding   Encoding
	Conversion *LinearConversion
	Unit       string
	Choices    []Choice
}

// Extent bounds the element count of a variable-length field. A field carrying
// one is the last field of its record, so no other field's offset depends on
// the count the ECU actually returns.
type Extent struct {
	MinCount uint32
	MaxCount uint32
}

// BitSize is the number of bits the field always occupies. A variable-length
// field can occupy up to MaxBitSize.
func (field Field) BitSize() uint32 {
	return field.BitLength * field.Count
}

// MaxBitSize is the largest number of bits the field can occupy.
func (field Field) MaxBitSize() uint32 {
	if field.Variable == nil {
		return field.BitSize()
	}
	return field.BitLength * field.Variable.MaxCount
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
