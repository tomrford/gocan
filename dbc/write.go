package dbc

import (
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Edit returns a new database made by applying edit to a copy of db. The copy
// is written as DBC text and parsed as name, so the result is validated like
// any parsed database, its lookups and codecs follow the edits, and Source
// returns the written text. db is not changed.
//
// edit may change DBC declarations, including adding or removing nodes,
// messages and signals. Message.Format is derived from VFrameFormat and
// ProtocolType attributes; edit those attributes to change the frame format.
// Diagnostics are also derived. Lookups on the copy describe it before edit ran.
func (db *Database) Edit(name string, edit func(*Database)) (*Database, error) {
	var text strings.Builder
	if err := db.Write(&text); err != nil {
		return nil, err
	}
	edited, err := Parse(name, text.String())
	if err != nil {
		return nil, err
	}
	edit(edited)
	text.Reset()
	if err := edited.Write(&text); err != nil {
		return nil, err
	}
	return Parse(name, text.String())
}

// Write writes db as UTF-8 DBC text. For a parsed database, parsing the text
// yields a database equal to db apart from Diagnostics and Source: records
// that Parse dropped are not written, and source formatting, number spelling
// and encoding are not kept. Equal databases write equal text.
//
// Write reads the exported DBC declarations, so it also accepts an edited
// copy, as Edit does. Message.Format is derived from attributes and is not
// written. It writes nothing and returns an error for an out-of-range CAN
// identifier or an unrepresentable name, string or number. Other
// inconsistencies are reported when the text is parsed.
//
// To write Windows-1252, wrap w with charmap.Windows1252.NewEncoder().Writer
// from golang.org/x/text/encoding/charmap.
func (db *Database) Write(w io.Writer) error {
	var err error
	fail := func(format string, args ...any) {
		if err == nil {
			err = fmt.Errorf("dbc: "+format, args...)
		}
	}
	name := func(s string) string {
		valid := s != "" && utf8.ValidString(s) && !isDigit(s[0]) && !strings.HasPrefix(s, "//") &&
			!(s[0] == '.' && len(s) > 1 && isDigit(s[1]))
		for i := 0; valid && i < len(s); i++ {
			valid = !isDelimiter(s[i])
		}
		if !valid {
			fail("name %q cannot be written as a DBC identifier", s)
		}
		return s
	}
	quote := func(s string) string {
		// \" is the only escape, so a final backslash would escape the
		// closing quote.
		if !utf8.ValidString(s) || strings.HasSuffix(s, `\`) {
			fail("text %q cannot be written as a DBC string", s)
		}
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	number := func(v float64) string {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			fail("number %v cannot be written", v)
		}
		// Shortest exact text, with an exponent only where Python's repr
		// (and so cantools) uses one.
		if magnitude := math.Abs(v); magnitude != 0 && (magnitude < 1e-4 || magnitude >= 1e16) {
			return strconv.FormatFloat(v, 'g', -1, 64)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	value := func(v AttributeValue) string {
		switch v.Kind {
		case AttributeKindString:
			return quote(v.Text)
		case AttributeKindEnum:
			// A numeric -1 has empty Text, even if the enum has an empty label.
			if v.Integer == -1 && v.Text != "" {
				return quote(v.Text)
			}
			return strconv.FormatInt(v.Integer, 10)
		case AttributeKindFloat:
			// A point keeps an undefined attribute inferred as a float.
			text := number(v.Float)
			if !strings.ContainsAny(text, ".e") {
				text += ".0"
			}
			return text
		default:
			return strconv.FormatInt(v.Integer, 10)
		}
	}
	rawID := func(message *Message) uint32 {
		if message.Extended {
			if message.ID > 0x1fffffff {
				fail("message %q extended identifier %#x exceeds the 29-bit CAN ID", message.Name, message.ID)
			}
			return message.ID | 0x80000000
		}
		if message.ID > 0x7ff {
			fail("message %q standard identifier %#x exceeds the 11-bit CAN ID", message.Name, message.ID)
		}
		return message.ID
	}

	var b strings.Builder
	fmt.Fprintf(&b, "VERSION %s\n\nNS_ :\n", quote(db.Version))
	for _, symbol := range []string{
		"NS_DESC_", "CM_", "BA_DEF_", "BA_", "VAL_", "CAT_DEF_", "CAT_", "FILTER", "BA_DEF_DEF_", "EV_DATA_",
		"ENVVAR_DATA_", "SGTYPE_", "SGTYPE_VAL_", "BA_DEF_SGTYPE_", "BA_SGTYPE_", "SIG_TYPE_REF_", "VAL_TABLE_",
		"SIG_GROUP_", "SIG_VALTYPE_", "SIGTYPE_VALTYPE_", "BO_TX_BU_", "BA_DEF_REL_", "BA_REL_", "BA_DEF_DEF_REL_",
		"BU_SG_REL_", "BU_EV_REL_", "BU_BO_REL_", "SG_MUL_VAL_",
	} {
		fmt.Fprintf(&b, "\t%s\n", symbol)
	}
	b.WriteString("\nBS_:\n\nBU_:")
	for _, node := range db.Nodes {
		b.WriteString(" " + name(node.Name))
	}
	b.WriteString("\n\n")

	for _, table := range db.ValueTables {
		b.WriteString("VAL_TABLE_ " + name(table.Name))
		for _, description := range table.Values {
			fmt.Fprintf(&b, " %d %s", description.Value, quote(description.Label))
		}
		b.WriteString(" ;\n")
	}
	if len(db.ValueTables) > 0 {
		b.WriteString("\n")
	}

	for index := range db.Messages {
		message := &db.Messages[index]
		transmitter := "Vector__XXX"
		if len(message.Transmitters) > 0 {
			transmitter = name(message.Transmitters[0])
		}
		fmt.Fprintf(&b, "BO_ %d %s: %d %s\n", rawID(message), name(message.Name), message.Length, transmitter)
		for _, signal := range message.Signals {
			marker := ""
			if signal.Multiplex != nil {
				if len(signal.Multiplex.Ranges) == 0 {
					fail("multiplexed signal %q has no selector values", signal.Name)
				} else {
					marker = " m" + strconv.FormatUint(signal.Multiplex.Ranges[0].First, 10)
				}
				if signal.IsMultiplexer {
					marker += "M"
				}
			} else if signal.IsMultiplexer {
				marker = " M"
			}
			order, sign := '0', '+'
			if signal.ByteOrder == ByteOrderLittleEndian {
				order = '1'
			}
			if signal.Signed {
				sign = '-'
			}
			fmt.Fprintf(&b, " SG_ %s%s : %d|%d@%c%c (%s,%s) [%s|%s] %s", name(signal.Name), marker,
				signal.StartBit, signal.BitLength, order, sign, number(signal.Factor), number(signal.Offset),
				number(signal.Minimum), number(signal.Maximum), quote(signal.Unit))
			for receiverIndex, receiver := range signal.Receivers {
				separator := ","
				if receiverIndex == 0 {
					separator = " "
				}
				b.WriteString(separator + name(receiver))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	for index := range db.Messages {
		message := &db.Messages[index]
		if len(message.Transmitters) < 2 {
			continue
		}
		fmt.Fprintf(&b, "BO_TX_BU_ %d :", rawID(message))
		for transmitterIndex, transmitter := range message.Transmitters {
			separator := ","
			if transmitterIndex == 0 {
				separator = " "
			}
			b.WriteString(separator + name(transmitter))
		}
		b.WriteString(";\n")
	}

	if db.Comment != "" {
		fmt.Fprintf(&b, "CM_ %s;\n", quote(db.Comment))
	}
	for _, node := range db.Nodes {
		if node.Comment != "" {
			fmt.Fprintf(&b, "CM_ BU_ %s %s;\n", name(node.Name), quote(node.Comment))
		}
	}
	for index := range db.Messages {
		message := &db.Messages[index]
		if message.Comment != "" {
			fmt.Fprintf(&b, "CM_ BO_ %d %s;\n", rawID(message), quote(message.Comment))
		}
		for _, signal := range message.Signals {
			if signal.Comment != "" {
				fmt.Fprintf(&b, "CM_ SG_ %d %s %s;\n", rawID(message), name(signal.Name), quote(signal.Comment))
			}
		}
	}

	for _, definition := range db.AttributeDefinitions {
		b.WriteString("BA_DEF_ ")
		switch definition.Scope {
		case AttributeScopeNode:
			b.WriteString("BU_ ")
		case AttributeScopeMessage:
			b.WriteString("BO_ ")
		case AttributeScopeSignal:
			b.WriteString("SG_ ")
		}
		b.WriteString(quote(definition.Name))
		switch definition.Kind {
		case AttributeKindInteger:
			fmt.Fprintf(&b, " INT %s %s", number(definition.Minimum), number(definition.Maximum))
		case AttributeKindHex:
			fmt.Fprintf(&b, " HEX %s %s", number(definition.Minimum), number(definition.Maximum))
		case AttributeKindFloat:
			fmt.Fprintf(&b, " FLOAT %s %s", number(definition.Minimum), number(definition.Maximum))
		case AttributeKindString:
			b.WriteString(" STRING")
		case AttributeKindEnum:
			b.WriteString(" ENUM")
			for choiceIndex, choice := range definition.Choices {
				separator := ","
				if choiceIndex == 0 {
					separator = " "
				}
				b.WriteString(separator + quote(choice))
			}
		default:
			fail("attribute %q has unknown kind %d", definition.Name, definition.Kind)
		}
		b.WriteString(";\n")
	}
	for _, definition := range db.AttributeDefinitions {
		if definition.Default == nil {
			continue
		}
		// Enum defaults use the label, as Vector tools write them, unless a
		// repeated label would resolve to another index.
		literal := value(*definition.Default)
		if def := definition.Default; def.Kind == AttributeKindEnum && int64(slices.Index(definition.Choices, def.Text)) == def.Integer {
			literal = quote(def.Text)
		}
		fmt.Fprintf(&b, "BA_DEF_DEF_ %s %s;\n", quote(definition.Name), literal)
	}
	for _, attribute := range slices.Sorted(maps.Keys(db.Attributes)) {
		fmt.Fprintf(&b, "BA_ %s %s;\n", quote(attribute), value(db.Attributes[attribute]))
	}
	for _, node := range db.Nodes {
		for _, attribute := range slices.Sorted(maps.Keys(node.Attributes)) {
			fmt.Fprintf(&b, "BA_ %s BU_ %s %s;\n", quote(attribute), name(node.Name), value(node.Attributes[attribute]))
		}
	}
	for index := range db.Messages {
		message := &db.Messages[index]
		for _, attribute := range slices.Sorted(maps.Keys(message.Attributes)) {
			fmt.Fprintf(&b, "BA_ %s BO_ %d %s;\n", quote(attribute), rawID(message), value(message.Attributes[attribute]))
		}
		for _, signal := range message.Signals {
			for _, attribute := range slices.Sorted(maps.Keys(signal.Attributes)) {
				fmt.Fprintf(&b, "BA_ %s SG_ %d %s %s;\n", quote(attribute), rawID(message), name(signal.Name), value(signal.Attributes[attribute]))
			}
		}
	}

	for index := range db.Messages {
		message := &db.Messages[index]
		for _, signal := range message.Signals {
			if len(signal.Values) == 0 {
				continue
			}
			fmt.Fprintf(&b, "VAL_ %d %s", rawID(message), name(signal.Name))
			for _, description := range signal.Values {
				fmt.Fprintf(&b, " %d %s", description.Value, quote(description.Label))
			}
			b.WriteString(" ;\n")
		}
	}
	for index := range db.Messages {
		message := &db.Messages[index]
		for _, signal := range message.Signals {
			switch signal.ValueType {
			case ValueTypeFloat32:
				fmt.Fprintf(&b, "SIG_VALTYPE_ %d %s : 1;\n", rawID(message), name(signal.Name))
			case ValueTypeFloat64:
				fmt.Fprintf(&b, "SIG_VALTYPE_ %d %s : 2;\n", rawID(message), name(signal.Name))
			}
		}
	}
	for index := range db.Messages {
		message := &db.Messages[index]
		for _, group := range message.SignalGroups {
			fmt.Fprintf(&b, "SIG_GROUP_ %d %s %d :", rawID(message), name(group.Name), group.Repetitions)
			for _, signal := range group.Signals {
				b.WriteString(" " + name(signal))
			}
			b.WriteString(";\n")
		}
	}

	// Simple multiplexing says only "selected by the message's one root
	// multiplexer at one value"; anything else lists every condition.
	for index := range db.Messages {
		message := &db.Messages[index]
		var roots []string
		for _, signal := range message.Signals {
			if signal.IsMultiplexer && signal.Multiplex == nil {
				roots = append(roots, signal.Name)
			}
		}
		simple := true
		for _, signal := range message.Signals {
			if condition := signal.Multiplex; condition != nil && (len(roots) != 1 || condition.Selector != roots[0] ||
				len(condition.Ranges) != 1 || condition.Ranges[0].First != condition.Ranges[0].Last) {
				simple = false
			}
		}
		if simple {
			continue
		}
		for _, signal := range message.Signals {
			if signal.Multiplex == nil {
				continue
			}
			fmt.Fprintf(&b, "SG_MUL_VAL_ %d %s %s", rawID(message), name(signal.Name), name(signal.Multiplex.Selector))
			for rangeIndex, selected := range signal.Multiplex.Ranges {
				separator := ","
				if rangeIndex == 0 {
					separator = ""
				}
				fmt.Fprintf(&b, "%s %d-%d", separator, selected.First, selected.Last)
			}
			b.WriteString(";\n")
		}
	}

	if err != nil {
		return err
	}
	_, err = io.WriteString(w, b.String())
	return err
}
