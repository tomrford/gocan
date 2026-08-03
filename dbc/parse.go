package dbc

import (
	"fmt"
	"strconv"
	"strings"
)

// Error reports invalid or unsupported DBC source.
type Error struct {
	Position Position
	Keyword  string
	Message  string
}

func (err *Error) Error() string {
	location := err.Position.Source
	if location == "" {
		location = "DBC input"
	}
	if err.Position.Line > 0 {
		location = fmt.Sprintf("%s:%d:%d", location, err.Position.Line, err.Position.Column)
	}
	if err.Keyword != "" {
		return fmt.Sprintf("%s: %s: %s", location, err.Keyword, err.Message)
	}
	return fmt.Sprintf("%s: %s", location, err.Message)
}

// Parse parses source into a resolved DBC model. Source must be UTF-8 text.
// Byte encoding detection belongs in the later file-loading layer.
// TODO: Add a file loader that detects common legacy encodings such as
// Windows-1252 without complicating the parser's text contract.
func Parse(name, source string) (*Database, error) {
	tokens, err := lex(name, source)
	if err != nil {
		return nil, err
	}
	raw, err := newParser(tokens).parse()
	if err != nil {
		return nil, err
	}
	return resolve(raw)
}

type rawDatabase struct {
	version           string
	nodes             []rawNode
	messages          []rawMessage
	valueTables       []rawValueTable
	valueLists        []rawValueList
	valueTypes        []rawValueType
	multiplex         []rawMultiplex
	transmitters      []rawTransmitters
	signalGroups      []rawSignalGroup
	comments          []rawComment
	attributeDefs     []rawAttributeDefinition
	attributeDefaults []rawAttributeDefault
	attributeValues   []rawAttributeAssignment
	diagnostics       []Diagnostic
}

type rawNode struct {
	name string
	pos  Position
}

type rawMessage struct {
	id          uint32
	name        string
	length      uint32
	transmitter string
	signals     []rawSignal
	pos         Position
}

type rawSignal struct {
	name      string
	muxMarker string
	startBit  uint32
	bitLength uint32
	byteOrder ByteOrder
	signed    bool
	factor    float64
	offset    float64
	minimum   float64
	maximum   float64
	unit      string
	receivers []string
	pos       Position
}

type rawValueTable struct {
	name   string
	values []ValueDescription
	pos    Position
}

type rawValueList struct {
	messageID uint32
	signal    string
	values    []ValueDescription
	pos       Position
}

type rawValueType struct {
	messageID uint32
	signal    string
	valueType ValueType
	pos       Position
}

type rawMultiplex struct {
	messageID uint32
	signal    string
	selector  string
	ranges    []MultiplexRange
	pos       Position
}

type rawTransmitters struct {
	messageID    uint32
	transmitters []string
	pos          Position
}

type rawSignalGroup struct {
	messageID   uint32
	name        string
	repetitions uint32
	signals     []string
	pos         Position
}

type rawComment struct {
	scope     AttributeScope
	messageID uint32
	object    string
	text      string
	pos       Position
}

type rawAttributeDefinition struct {
	name    string
	scope   AttributeScope
	kind    AttributeKind
	minimum float64
	maximum float64
	choices []string
	pos     Position
}

type rawAttributeDefault struct {
	name  string
	value rawLiteral
	pos   Position
}

type rawAttributeAssignment struct {
	name      string
	scope     AttributeScope
	messageID uint32
	object    string
	value     rawLiteral
	pos       Position
}

type rawLiteral struct {
	text     string
	isString bool
	pos      Position
}

type parser struct {
	tokens         []token
	index          int
	currentMessage int
	keyword        string
	raw            rawDatabase
}

func newParser(tokens []token) *parser {
	return &parser{tokens: tokens, currentMessage: -1}
}

func (p *parser) parse() (rawDatabase, error) {
	for {
		p.skipNewlines()
		if p.peek().kind == tokenEOF {
			return p.raw, nil
		}

		p.keyword = ""
		keyword, err := p.take(tokenAtom, "expected DBC record keyword")
		if err != nil {
			return rawDatabase{}, err
		}
		p.keyword = keyword.text
		p.currentMessage = p.keepCurrentMessage(keyword.text)

		switch keyword.text {
		case "VERSION":
			err = p.parseVersion()
		case "NS_":
			err = p.parseNewSymbols()
		case "BS_":
			p.skipRecord()
		case "BU_":
			err = p.parseNodes()
		case "BO_":
			err = p.parseMessage(keyword)
		case "SG_":
			err = p.parseSignal(keyword)
		case "VAL_":
			err = p.parseValueList(keyword)
		case "VAL_TABLE_":
			err = p.parseValueTable(keyword)
		case "SIG_VALTYPE_":
			err = p.parseSignalValueType(keyword)
		case "SG_MUL_VAL_":
			err = p.parseExtendedMultiplex(keyword)
		case "BO_TX_BU_":
			err = p.parseMessageTransmitters(keyword)
		case "SIG_GROUP_":
			err = p.parseSignalGroup(keyword)
		case "CM_":
			err = p.parseComment(keyword)
		case "BA_DEF_":
			err = p.parseAttributeDefinition(keyword)
		case "BA_DEF_DEF_":
			err = p.parseAttributeDefault(keyword)
		case "BA_":
			err = p.parseAttributeAssignment(keyword)
		case "SGTYPE_", "SGTYPE_VAL_", "SIG_TYPE_REF_", "SIGTYPE_VALTYPE_", "BA_DEF_SGTYPE_", "BA_SGTYPE_":
			err = p.errorAt(keyword, "unsupported signal-type records can change signal decoding")
		case "EV_", "ENVVAR_DATA_", "EV_DATA_", "CAT_DEF_", "CAT_", "FILTER",
			"BA_DEF_REL_", "BA_REL_", "BA_DEF_DEF_REL_", "BU_SG_REL_", "BU_EV_REL_", "BU_BO_REL_":
			p.raw.diagnostics = append(p.raw.diagnostics, Diagnostic{
				Position: keyword.pos,
				Keyword:  keyword.text,
				Message:  "record is not represented in the resolved model",
			})
			p.skipRecord()
		default:
			p.raw.diagnostics = append(p.raw.diagnostics, Diagnostic{
				Position: keyword.pos,
				Keyword:  keyword.text,
				Message:  "unknown record was ignored",
			})
			p.skipRecord()
		}
		if err != nil {
			return rawDatabase{}, err
		}
	}
}

func (p *parser) keepCurrentMessage(keyword string) int {
	if keyword == "SG_" {
		return p.currentMessage
	}
	return -1
}

func (p *parser) parseVersion() error {
	value, err := p.take(tokenString, "expected quoted version")
	if err != nil {
		return err
	}
	p.raw.version = value.text
	return p.endLine()
}

func (p *parser) parseNewSymbols() error {
	if _, err := p.take(tokenColon, "expected ':' after NS_"); err != nil {
		return err
	}
	p.skipLine()
	for p.peek().kind == tokenNewline {
		p.index++
		if p.peek().kind == tokenEOF || p.peek().pos.Column == 1 {
			break
		}
		p.skipLine()
	}
	return nil
}

func (p *parser) parseNodes() error {
	if _, err := p.take(tokenColon, "expected ':' after BU_"); err != nil {
		return err
	}
	for p.peek().kind != tokenNewline && p.peek().kind != tokenEOF {
		node, err := p.take(tokenAtom, "expected node name")
		if err != nil {
			return err
		}
		p.raw.nodes = append(p.raw.nodes, rawNode{name: node.text, pos: node.pos})
	}
	return p.endLine()
}

func (p *parser) parseMessage(keyword token) error {
	id, err := p.parseUint32("message ID")
	if err != nil {
		return err
	}
	name, err := p.take(tokenAtom, "expected message name")
	if err != nil {
		return err
	}
	if _, err = p.take(tokenColon, "expected ':' after message name"); err != nil {
		return err
	}
	length, err := p.parseUint32("message length")
	if err != nil {
		return err
	}
	transmitter, err := p.take(tokenAtom, "expected message transmitter")
	if err != nil {
		return err
	}
	if err = p.endLine(); err != nil {
		return err
	}
	p.raw.messages = append(p.raw.messages, rawMessage{
		id: id, name: name.text, length: length, transmitter: transmitter.text, pos: keyword.pos,
	})
	p.currentMessage = len(p.raw.messages) - 1
	return nil
}

func (p *parser) parseSignal(keyword token) error {
	if p.currentMessage < 0 {
		return p.errorAt(keyword, "signal is not attached to a preceding message")
	}
	name, err := p.take(tokenAtom, "expected signal name")
	if err != nil {
		return err
	}
	muxMarker := ""
	if p.peek().kind == tokenAtom {
		muxMarker = p.next().text
	}
	if _, err = p.take(tokenColon, "expected ':' before signal layout"); err != nil {
		return err
	}
	startBit, err := p.parseUint32("signal start bit")
	if err != nil {
		return err
	}
	if _, err = p.take(tokenPipe, "expected '|' after signal start bit"); err != nil {
		return err
	}
	bitLength, err := p.parseUint32("signal bit length")
	if err != nil {
		return err
	}
	if _, err = p.take(tokenAt, "expected '@' after signal bit length"); err != nil {
		return err
	}
	order, err := p.take(tokenNumber, "expected DBC byte order 0 or 1")
	if err != nil {
		return err
	}
	var byteOrder ByteOrder
	switch order.text {
	case "0":
		byteOrder = ByteOrderBigEndian
	case "1":
		byteOrder = ByteOrderLittleEndian
	default:
		return p.errorAt(order, "byte order must be 0 or 1")
	}
	sign := p.next()
	if sign.kind != tokenPlus && sign.kind != tokenMinus {
		return p.errorAt(sign, "expected '+' or '-' signal signedness")
	}
	if _, err = p.take(tokenLeftParen, "expected '(' before factor"); err != nil {
		return err
	}
	factor, err := p.parseFloat("signal factor")
	if err != nil {
		return err
	}
	if _, err = p.take(tokenComma, "expected ',' between factor and offset"); err != nil {
		return err
	}
	offset, err := p.parseFloat("signal offset")
	if err != nil {
		return err
	}
	if _, err = p.take(tokenRightParen, "expected ')' after offset"); err != nil {
		return err
	}
	if _, err = p.take(tokenLeftBracket, "expected '[' before signal range"); err != nil {
		return err
	}
	minimum, err := p.parseFloat("signal minimum")
	if err != nil {
		return err
	}
	if _, err = p.take(tokenPipe, "expected '|' in signal range"); err != nil {
		return err
	}
	maximum, err := p.parseFloat("signal maximum")
	if err != nil {
		return err
	}
	if _, err = p.take(tokenRightBracket, "expected ']' after signal range"); err != nil {
		return err
	}
	unit, err := p.take(tokenString, "expected quoted signal unit")
	if err != nil {
		return err
	}
	var receivers []string
	for p.peek().kind != tokenNewline && p.peek().kind != tokenEOF {
		if p.peek().kind == tokenComma {
			p.index++
			continue
		}
		receiver, err := p.take(tokenAtom, "expected receiver name")
		if err != nil {
			return err
		}
		receivers = append(receivers, receiver.text)
	}
	if err = p.endLine(); err != nil {
		return err
	}
	message := &p.raw.messages[p.currentMessage]
	message.signals = append(message.signals, rawSignal{
		name: name.text, muxMarker: muxMarker, startBit: startBit, bitLength: bitLength,
		byteOrder: byteOrder, signed: sign.kind == tokenMinus, factor: factor, offset: offset,
		minimum: minimum, maximum: maximum, unit: unit.text, receivers: receivers, pos: keyword.pos,
	})
	return nil
}

func (p *parser) parseValueList(keyword token) error {
	if p.peek().kind != tokenNumber {
		p.raw.diagnostics = append(p.raw.diagnostics, Diagnostic{
			Position: keyword.pos, Keyword: keyword.text, Message: "environment-variable value list was ignored",
		})
		p.skipRecord()
		return nil
	}
	messageID, err := p.parseUint32("message ID")
	if err != nil {
		return err
	}
	signal, err := p.take(tokenAtom, "expected signal name")
	if err != nil {
		return err
	}
	values, err := p.parseValueDescriptions()
	if err != nil {
		return err
	}
	p.raw.valueLists = append(p.raw.valueLists, rawValueList{
		messageID: messageID, signal: signal.text, values: values, pos: keyword.pos,
	})
	return nil
}

func (p *parser) parseValueTable(keyword token) error {
	name, err := p.take(tokenAtom, "expected value-table name")
	if err != nil {
		return err
	}
	values, err := p.parseValueDescriptions()
	if err != nil {
		return err
	}
	p.raw.valueTables = append(p.raw.valueTables, rawValueTable{name: name.text, values: values, pos: keyword.pos})
	return nil
}

func (p *parser) parseValueDescriptions() ([]ValueDescription, error) {
	var values []ValueDescription
	for p.peek().kind != tokenSemicolon && p.peek().kind != tokenNewline && p.peek().kind != tokenEOF {
		value, _, err := p.parseInt64("raw value")
		if err != nil {
			return nil, err
		}
		label, err := p.take(tokenString, "expected quoted value description")
		if err != nil {
			return nil, err
		}
		values = append(values, ValueDescription{Value: value, Label: label.text})
	}
	if p.peek().kind == tokenSemicolon {
		p.index++
	}
	return values, p.endLine()
}

func (p *parser) parseSignalValueType(keyword token) error {
	messageID, err := p.parseUint32("message ID")
	if err != nil {
		return err
	}
	signal, err := p.take(tokenAtom, "expected signal name")
	if err != nil {
		return err
	}
	if _, err = p.take(tokenColon, "expected ':' before signal value type"); err != nil {
		return err
	}
	rawType, err := p.take(tokenNumber, "expected signal value type")
	if err != nil {
		return err
	}
	var valueType ValueType
	switch rawType.text {
	case "0":
		valueType = ValueTypeInteger
	case "1":
		valueType = ValueTypeFloat32
	case "2":
		valueType = ValueTypeFloat64
	default:
		return p.errorAt(rawType, "signal value type must be 0, 1, or 2")
	}
	if err = p.endStatement(); err != nil {
		return err
	}
	p.raw.valueTypes = append(p.raw.valueTypes, rawValueType{
		messageID: messageID, signal: signal.text, valueType: valueType, pos: keyword.pos,
	})
	return nil
}

func (p *parser) parseExtendedMultiplex(keyword token) error {
	messageID, err := p.parseUint32("message ID")
	if err != nil {
		return err
	}
	signal, err := p.take(tokenAtom, "expected multiplexed signal name")
	if err != nil {
		return err
	}
	selector, err := p.take(tokenAtom, "expected multiplexer signal name")
	if err != nil {
		return err
	}
	var ranges []MultiplexRange
	for {
		first, err := p.parseUint64("multiplexer range start")
		if err != nil {
			return err
		}
		if _, err = p.take(tokenMinus, "expected '-' in multiplexer range"); err != nil {
			return err
		}
		last, err := p.parseUint64("multiplexer range end")
		if err != nil {
			return err
		}
		if first > last {
			return p.errorAt(keyword, "multiplexer range start exceeds its end")
		}
		ranges = append(ranges, MultiplexRange{First: first, Last: last})
		if p.peek().kind != tokenComma {
			break
		}
		p.index++
	}
	if err = p.endStatement(); err != nil {
		return err
	}
	p.raw.multiplex = append(p.raw.multiplex, rawMultiplex{
		messageID: messageID, signal: signal.text, selector: selector.text, ranges: ranges, pos: keyword.pos,
	})
	return nil
}

func (p *parser) parseMessageTransmitters(keyword token) error {
	messageID, err := p.parseUint32("message ID")
	if err != nil {
		return err
	}
	if _, err = p.take(tokenColon, "expected ':' before transmitters"); err != nil {
		return err
	}
	var transmitters []string
	for p.peek().kind != tokenSemicolon && p.peek().kind != tokenNewline && p.peek().kind != tokenEOF {
		if p.peek().kind == tokenComma {
			p.index++
			continue
		}
		name, err := p.take(tokenAtom, "expected transmitter name")
		if err != nil {
			return err
		}
		transmitters = append(transmitters, name.text)
	}
	if err = p.endStatement(); err != nil {
		return err
	}
	p.raw.transmitters = append(p.raw.transmitters, rawTransmitters{
		messageID: messageID, transmitters: transmitters, pos: keyword.pos,
	})
	return nil
}

func (p *parser) parseSignalGroup(keyword token) error {
	messageID, err := p.parseUint32("message ID")
	if err != nil {
		return err
	}
	name, err := p.take(tokenAtom, "expected signal-group name")
	if err != nil {
		return err
	}
	repetitions, err := p.parseUint32("signal-group repetitions")
	if err != nil {
		return err
	}
	if _, err = p.take(tokenColon, "expected ':' before grouped signals"); err != nil {
		return err
	}
	var signals []string
	for p.peek().kind != tokenSemicolon && p.peek().kind != tokenNewline && p.peek().kind != tokenEOF {
		signal, err := p.take(tokenAtom, "expected grouped signal name")
		if err != nil {
			return err
		}
		signals = append(signals, signal.text)
	}
	if err = p.endStatement(); err != nil {
		return err
	}
	p.raw.signalGroups = append(p.raw.signalGroups, rawSignalGroup{
		messageID: messageID, name: name.text, repetitions: repetitions, signals: signals, pos: keyword.pos,
	})
	return nil
}

func (p *parser) parseComment(keyword token) error {
	comment := rawComment{scope: AttributeScopeDatabase, pos: keyword.pos}
	if p.peek().kind == tokenString {
		comment.text = p.next().text
	} else {
		target, err := p.take(tokenAtom, "expected comment target")
		if err != nil {
			return err
		}
		switch target.text {
		case "BU_":
			comment.scope = AttributeScopeNode
			name, err := p.take(tokenAtom, "expected node name")
			if err != nil {
				return err
			}
			comment.object = name.text
		case "BO_":
			comment.scope = AttributeScopeMessage
			comment.messageID, err = p.parseUint32("message ID")
			if err != nil {
				return err
			}
		case "SG_":
			comment.scope = AttributeScopeSignal
			comment.messageID, err = p.parseUint32("message ID")
			if err != nil {
				return err
			}
			name, err := p.take(tokenAtom, "expected signal name")
			if err != nil {
				return err
			}
			comment.object = name.text
		case "EV_":
			p.raw.diagnostics = append(p.raw.diagnostics, Diagnostic{
				Position: keyword.pos, Keyword: keyword.text, Message: "environment-variable comment was ignored",
			})
			p.skipRecord()
			return nil
		default:
			return p.errorAt(target, "unsupported comment target")
		}
		text, err := p.take(tokenString, "expected quoted comment")
		if err != nil {
			return err
		}
		comment.text = text.text
	}
	if err := p.endStatement(); err != nil {
		return err
	}
	p.raw.comments = append(p.raw.comments, comment)
	return nil
}

func (p *parser) parseAttributeDefinition(keyword token) error {
	scope := AttributeScopeDatabase
	if p.peek().kind == tokenAtom {
		switch p.peek().text {
		case "BU_":
			scope = AttributeScopeNode
			p.index++
		case "BO_":
			scope = AttributeScopeMessage
			p.index++
		case "SG_":
			scope = AttributeScopeSignal
			p.index++
		case "EV_":
			p.raw.diagnostics = append(p.raw.diagnostics, Diagnostic{
				Position: keyword.pos, Keyword: keyword.text, Message: "environment-variable attribute definition was ignored",
			})
			p.skipRecord()
			return nil
		}
	}
	name, err := p.take(tokenString, "expected quoted attribute name")
	if err != nil {
		return err
	}
	kindToken, err := p.take(tokenAtom, "expected attribute type")
	if err != nil {
		return err
	}
	definition := rawAttributeDefinition{name: name.text, scope: scope, pos: keyword.pos}
	switch kindToken.text {
	case "INT":
		definition.kind = AttributeKindInteger
		definition.minimum, err = p.parseFloat("attribute minimum")
		if err == nil {
			definition.maximum, err = p.parseFloat("attribute maximum")
		}
	case "HEX":
		definition.kind = AttributeKindHex
		definition.minimum, err = p.parseFloat("attribute minimum")
		if err == nil {
			definition.maximum, err = p.parseFloat("attribute maximum")
		}
	case "FLOAT":
		definition.kind = AttributeKindFloat
		definition.minimum, err = p.parseFloat("attribute minimum")
		if err == nil {
			definition.maximum, err = p.parseFloat("attribute maximum")
		}
	case "STRING":
		definition.kind = AttributeKindString
	case "ENUM":
		definition.kind = AttributeKindEnum
		for p.peek().kind != tokenSemicolon && p.peek().kind != tokenNewline && p.peek().kind != tokenEOF {
			if p.peek().kind == tokenComma {
				p.index++
				continue
			}
			choice, takeErr := p.take(tokenString, "expected quoted enum choice")
			if takeErr != nil {
				return takeErr
			}
			definition.choices = append(definition.choices, choice.text)
		}
	default:
		return p.errorAt(kindToken, "unsupported attribute type")
	}
	if err != nil {
		return err
	}
	if err = p.endStatement(); err != nil {
		return err
	}
	p.raw.attributeDefs = append(p.raw.attributeDefs, definition)
	return nil
}

func (p *parser) parseAttributeDefault(keyword token) error {
	name, err := p.take(tokenString, "expected quoted attribute name")
	if err != nil {
		return err
	}
	value, err := p.parseLiteral()
	if err != nil {
		return err
	}
	if err = p.endStatement(); err != nil {
		return err
	}
	p.raw.attributeDefaults = append(p.raw.attributeDefaults, rawAttributeDefault{
		name: name.text, value: value, pos: keyword.pos,
	})
	return nil
}

func (p *parser) parseAttributeAssignment(keyword token) error {
	name, err := p.take(tokenString, "expected quoted attribute name")
	if err != nil {
		return err
	}
	assignment := rawAttributeAssignment{name: name.text, scope: AttributeScopeDatabase, pos: keyword.pos}
	if p.peek().kind == tokenAtom {
		target := p.peek().text
		switch target {
		case "BU_":
			p.index++
			assignment.scope = AttributeScopeNode
			object, takeErr := p.take(tokenAtom, "expected node name")
			if takeErr != nil {
				return takeErr
			}
			assignment.object = object.text
		case "BO_":
			p.index++
			assignment.scope = AttributeScopeMessage
			assignment.messageID, err = p.parseUint32("message ID")
		case "SG_":
			p.index++
			assignment.scope = AttributeScopeSignal
			assignment.messageID, err = p.parseUint32("message ID")
			if err == nil {
				object, takeErr := p.take(tokenAtom, "expected signal name")
				if takeErr != nil {
					return takeErr
				}
				assignment.object = object.text
			}
		case "EV_":
			p.raw.diagnostics = append(p.raw.diagnostics, Diagnostic{
				Position: keyword.pos, Keyword: keyword.text, Message: "environment-variable attribute was ignored",
			})
			p.skipRecord()
			return nil
		}
	}
	if err != nil {
		return err
	}
	assignment.value, err = p.parseLiteral()
	if err != nil {
		return err
	}
	if err = p.endStatement(); err != nil {
		return err
	}
	p.raw.attributeValues = append(p.raw.attributeValues, assignment)
	return nil
}

func (p *parser) parseLiteral() (rawLiteral, error) {
	if p.peek().kind == tokenString {
		tok := p.next()
		return rawLiteral{text: tok.text, isString: true, pos: tok.pos}, nil
	}
	sign := ""
	pos := p.peek().pos
	if p.peek().kind == tokenPlus || p.peek().kind == tokenMinus {
		sign = p.next().text
	}
	value := p.next()
	if value.kind != tokenNumber && value.kind != tokenAtom {
		return rawLiteral{}, p.errorAt(value, "expected attribute value")
	}
	return rawLiteral{text: sign + value.text, pos: pos}, nil
}

func (p *parser) parseUint32(field string) (uint32, error) {
	value, err := p.parseUint64(field)
	if err != nil {
		return 0, err
	}
	if value > uint64(^uint32(0)) {
		return 0, p.errorAt(p.previous(), field+" exceeds uint32")
	}
	return uint32(value), nil
}

func (p *parser) parseUint64(field string) (uint64, error) {
	tok, err := p.take(tokenNumber, "expected "+field)
	if err != nil {
		return 0, err
	}
	if strings.ContainsAny(tok.text, ".eE") {
		return 0, p.errorAt(tok, field+" must be an unsigned integer")
	}
	value, parseErr := strconv.ParseUint(tok.text, 10, 64)
	if parseErr != nil {
		return 0, p.errorAt(tok, "invalid "+field)
	}
	return value, nil
}

func (p *parser) parseInt64(field string) (int64, Position, error) {
	sign := ""
	pos := p.peek().pos
	if p.peek().kind == tokenPlus || p.peek().kind == tokenMinus {
		sign = p.next().text
	}
	tok, err := p.take(tokenNumber, "expected "+field)
	if err != nil {
		return 0, pos, err
	}
	if strings.ContainsAny(tok.text, ".eE") {
		return 0, pos, p.errorAt(tok, field+" must be an integer")
	}
	value, parseErr := strconv.ParseInt(sign+tok.text, 10, 64)
	if parseErr != nil {
		return 0, pos, p.errorAt(tok, "invalid "+field)
	}
	return value, pos, nil
}

func (p *parser) parseFloat(field string) (float64, error) {
	sign := ""
	if p.peek().kind == tokenPlus || p.peek().kind == tokenMinus {
		sign = p.next().text
	}
	tok, err := p.take(tokenNumber, "expected "+field)
	if err != nil {
		return 0, err
	}
	value, parseErr := strconv.ParseFloat(sign+tok.text, 64)
	if parseErr != nil {
		return 0, p.errorAt(tok, "invalid "+field)
	}
	return value, nil
}

func (p *parser) endStatement() error {
	if _, err := p.take(tokenSemicolon, "expected ';'"); err != nil {
		return err
	}
	return p.endLine()
}

func (p *parser) endLine() error {
	if p.peek().kind == tokenNewline {
		p.index++
		return nil
	}
	if p.peek().kind == tokenEOF {
		return nil
	}
	return p.errorAt(p.peek(), "unexpected token at end of "+p.keyword+" record")
}

func (p *parser) skipRecord() {
	for p.peek().kind != tokenEOF && p.peek().kind != tokenNewline {
		if p.next().kind == tokenSemicolon {
			break
		}
	}
	if p.peek().kind == tokenNewline {
		p.index++
	}
}

func (p *parser) skipLine() {
	for p.peek().kind != tokenEOF && p.peek().kind != tokenNewline {
		p.index++
	}
}

func (p *parser) skipNewlines() {
	for p.peek().kind == tokenNewline {
		p.index++
	}
}

func (p *parser) take(kind tokenKind, message string) (token, error) {
	tok := p.next()
	if tok.kind != kind {
		return token{}, p.errorAt(tok, message)
	}
	return tok, nil
}

func (p *parser) next() token {
	tok := p.peek()
	if p.index < len(p.tokens)-1 {
		p.index++
	}
	return tok
}

func (p *parser) peek() token {
	return p.tokens[p.index]
}

func (p *parser) previous() token {
	if p.index == 0 {
		return p.tokens[0]
	}
	return p.tokens[p.index-1]
}

func (p *parser) errorAt(tok token, message string) error {
	return &Error{Position: tok.pos, Keyword: p.keyword, Message: message}
}
