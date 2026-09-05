// Package cdd reads CANdela documents into selected diagnostic service catalogs.
// It preserves source metadata and reports unsupported service/message layouts.
// Data-record codecs support byte-aligned integers up to 64 bits, 32/64-bit
// floats, 8-bit ASCII, fixed arrays, and a variable array in final position.
// Complete service-message encoding and ECU execution are outside this package.
package cdd

import "github.com/tomrford/gocan/internal/scalar"

// Database is the catalog for one selected ECU/variant. Entries, instances,
// services and DIDs remain in source order. DIDs are views over the same service
// objects, not separate resolutions. Treat all catalog objects as read-only.
//
// States retains every declared state in document order. Sessions and
// SecurityLevels contain the corresponding CDD groups, including locked states.
// They describe the document, not UDS subfunctions or access privileges.
type Database struct {
	Selection      Selection
	ECU            Metadata
	Variant        Metadata
	Entries        []Entry
	DIDs           []*DID
	States         []State
	Sessions       []State
	SecurityLevels []State
	Diagnostics    []Diagnostic

	didsByName       map[string]int
	didsByIdentifier map[uint16]int
}

// Diagnostic reports an incomplete part of a catalog. Path uses one-based XML
// child positions, so it identifies sources even without unique qualifiers or
// IDs. An affected service remains in its instance. Kind distinguishes reference,
// message, codec, precondition and DID-lookup problems.
type Diagnostic struct {
	Path    string
	Source  SourceIdentity
	Kind    DiagnosticKind
	Message string
}

// DiagnosticKind identifies the part of resolution that needs attention.
type DiagnosticKind string

const (
	DiagnosticReference    DiagnosticKind = "reference"
	DiagnosticMessage      DiagnosticKind = "message"
	DiagnosticCodec        DiagnosticKind = "codec"
	DiagnosticPrecondition DiagnosticKind = "precondition"
	DiagnosticDID          DiagnosticKind = "did"
)

// Entry is one direct diagnostic child of the selected variant. Exactly one
// of Class or Instance is set. Entries preserves their interleaved source order;
// standalone instances are not assigned to an invented class.
type Entry struct {
	Class    *Class
	Instance *Instance
}

// Class groups diagnostic instances. Template metadata remains separate from
// the class's declared text. Err reports an unresolved template reference;
// compatibility with each instance's own template is not inferred.
type Class struct {
	Metadata
	TemplateRef string
	Template    *Metadata
	Instances   []*Instance
	Err         error
}

// Instance groups the services declared by one DIAGINST. A missing or invalid
// template does not remove the instance or its services.
type Instance struct {
	Metadata
	TemplateRef string
	Template    *Metadata
	Services    []*Service
	Err         error
}

// Service is one SERVICE child, including services outside the codec subset.
// Index is its one-based position in the instance. ServiceID comes from the
// protocol service's request SID; it does not by itself identify a protocol as
// UDS. Err reports a broken template binding or an unresolved request SID.
//
// Requirements belong to this service only. Request and PositiveResponse are
// nil when no such message is declared in a resolved binding; check Err before
// interpreting nil as absence. Message.Err and Record.CodecError expose layout
// and codec coverage separately. No whole-message encoder is supplied.
// Transitions retains the literal trans attribute without resolving it.
type Service struct {
	Metadata
	Index            int
	TemplateRef      string
	Template         *Metadata
	ProtocolRef      string
	Protocol         *Metadata
	ServiceID        *uint8
	Requirements     Precondition
	Transitions      *string
	Request          *Message
	PositiveResponse *Message
	Err              error
}

// Message describes a REQ or POS. Parameters retains constant/static components
// in message order, including their literal values and resolution errors.
// Record is the data portion, excluding SID, identifier and subfunction bytes.
// A nil Record with nil Err means no data component was declared. Err reports
// unsupported components or a record that cannot be laid out. A resolved Record
// may still have CodecError; its metadata remains available in that case.
type Message struct {
	Metadata
	Parameters []Parameter
	Record     *Record
	Err        error
}

// Parameter describes a CONSTCOMP or STATICCOMP. Spec is the literal CDD role
// (such as sid, sub, accm, or id), not an inferred UDS type. Value preserves the
// literal v attribute; NumericValue is set only when it fits the declared width
// as an unsigned integer. Static is the SHSTATIC binding when present.
type Parameter struct {
	Metadata
	Spec         string
	Static       *Metadata
	Datatype     *Metadata
	Value        *string
	NumericValue *uint64
	BitLength    uint32
	Err          error
}

// DID is a convenience view of literal SID 0x22 (Read) and 0x2e (Write) services
// with a consistent 16-bit spec="id" parameter in REQ, POS, or both. This shape
// does not prove that the ECU uses UDS; callers must establish the protocol
// before using UDS helpers. Every alternative keeps its own layout/requirements.
// Read or Write is empty if no matching service is bound. Invalid layouts remain
// represented. Instance points into Entries.
type DID struct {
	Instance   *Instance
	Identifier uint16
	Read       []*Service
	Write      []*Service
}

// Record describes only a service data record. Fields are in layout order.
// Length and MaxLength bound its byte size; they differ only for a final
// variable-length field. Metadata comes from its component container.
// Encode and Decode trust the parsed layout; treat it as read-only.
type Record struct {
	Metadata
	Name      string
	Length    uint32
	MaxLength uint32
	Fields    []Field

	codec *recordCodec
}

// State is a literal CDD state and its containing group. Index is its one-based
// position across all STATEGROUPS/STATE entries, including unsupported groups.
// GroupIndex is the one-based position of its STATEGROUP. Parse interprets
// mayBeExec entries as Index values, as observed in public Vector examples.
// Neither index is a UDS subfunction. No initial state is inferred.
type State struct {
	Metadata
	Index      int
	Group      Metadata
	GroupIndex int
	GroupSpec  string
}

// Precondition describes the containing service's execution requirements.
// Check Err first: a non-nil error means unknown requirements, not ECU rejection.
// Sessions and SecurityLevels contain literal states named by an explicit
// instance mayBeExec list, in document order with repeated references removed.
// A nil list means no states were listed for that group, not ECU permission.
// A service without a rule has nil lists and nil Err. Callers manage ECU state.
//
// Raw rule pointers distinguish missing attributes from explicit empty values.
// Exclusions, template rules, malformed/empty lists, unsupported state groups
// and unresolved service-template bindings remain unknown. These retain raw rules
// and have nil resolved lists. A locked state remains literal; no hierarchy,
// initial state, template precedence or inheritance is inferred.
type Precondition struct {
	Sessions                     []State
	SecurityLevels               []State
	MayBeExec                    *string
	NotExecInStateGroups         *string
	TemplateMayBeExec            *string
	TemplateNotExecInStateGroups *string
	Err                          error
}

// Values maps record field names to their physical values. Encoding accepts Go
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

// LinearConversion maps physical = raw*Scale + Offset. Scale is the CDD
// factor divided by its divisor; Offset is outside the division.
type LinearConversion struct {
	Scale  float64
	Offset float64

	// Raw limits use ordered integer keys, preserving all 64 bits even for
	// identity conversions. They are independent of physical scaling.
	minimum, maximum *uint64
}

// Choice assigns a label to one exact coded integer value. It shares its
// identity with the DBC value-description type, so label metadata moves
// between the two catalogs without conversion.
type Choice = scalar.Choice

// Field describes one fixed-size coded field. BitOffset is a linear offset
// from the start of the service data record; it does not use DBC bit numbering.
//
// BitLength is the width of a single element and Count its repetition, so the
// field occupies BitLength*Count bits. Count is 1 for scalars and greater for
// fixed-size arrays such as ASCII serial numbers and calibration blocks.
//
// Variable is set when the element count is only known from the length of the
// response. Count then repeats Variable.MinCount, so BitSize remains the number
// of bits the field always occupies.
type Field struct {
	Metadata
	// Datatype retains the declared type's own identity and presentation.
	Datatype Metadata
	// Groups is the outer-to-inner grouping path. A STRUCT contributes its
	// metadata; a DIDDATAREF contributes reference then shared-DID metadata.
	// Codec field keys remain flat qualifiers.
	Groups     []Metadata
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

// DIDByName returns the DID with this instance qualifier. Missing or ambiguous
// qualifiers return false; every entry remains available in DIDs.
func (database *Database) DIDByName(name string) (*DID, bool) {
	if database == nil {
		return nil, false
	}
	index, ok := database.didsByName[name]
	if !ok || index < 0 {
		return nil, false
	}
	return database.DIDs[index], true
}

// DIDByIdentifier returns the DID with identifier. Repeated identifiers return
// false; every entry remains available in DIDs.
func (database *Database) DIDByIdentifier(identifier uint16) (*DID, bool) {
	if database == nil {
		return nil, false
	}
	index, ok := database.didsByIdentifier[identifier]
	if !ok || index < 0 {
		return nil, false
	}
	return database.DIDs[index], true
}
