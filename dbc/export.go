package dbc

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// MarshalText serialises the resolved model as UTF-8 DBC. It preserves the
// represented definitions, attributes and decoding semantics, not the original
// source. Parse does not retain comments, bit timing, unsupported records or
// formatting; consult Diagnostics for reported omissions from the import.
//
// Edited and programmatic models must be representable by Parse, including
// consistent frame-format attributes. Names must be ASCII DBC identifiers of
// at most 32 characters. Invalid names, unrepresentable values or
// changes that would be lost on import return an error and no output. Empty
// transmitter/receiver lists use DBC's Vector__XXX placeholder. Export does not
// update the model's lookup tables or codecs; reparse to use an edited model.
// Literal backslash sequences interpreted as escapes by Parse are rejected
// when they cannot be represented consistently in other DBC readers.
func (db *Database) MarshalText() ([]byte, error) {
	if db == nil {
		return nil, fmt.Errorf("cannot export a nil DBC database")
	}
	if err := exportNames(db); err != nil {
		return nil, err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "VERSION %s\n\nNS_ :\n\nBS_:\n\nBU_:", dbcQuote(db.Version))
	for _, node := range db.Nodes {
		fmt.Fprintf(&out, " %s", node.Name)
	}
	out.WriteString("\n")
	for _, message := range db.Messages {
		id := exportID(message)
		senders := exportNodes(message.Transmitters)
		fmt.Fprintf(&out, "\nBO_ %d %s: %d %s\n", id, message.Name, message.Length, senders[0])
		for _, signal := range message.Signals {
			mux := ""
			if signal.Multiplex != nil {
				if len(signal.Multiplex.Ranges) == 0 {
					return nil, fmt.Errorf("signal %s has no multiplex ranges", signal.Name)
				}
				mux = "m" + strconv.FormatUint(signal.Multiplex.Ranges[0].First, 10)
			}
			if signal.IsMultiplexer {
				mux += "M"
			}
			sign := "+"
			if signal.Signed {
				sign = "-"
			}
			fmt.Fprintf(&out, " SG_ %s %s : %d|%d@%d%s (%s,%s) [%s|%s] %s %s\n",
				signal.Name, mux, signal.StartBit, signal.BitLength, signal.ByteOrder, sign,
				dbcNumber(signal.Factor), dbcNumber(signal.Offset), dbcNumber(signal.Minimum),
				dbcNumber(signal.Maximum), dbcQuote(signal.Unit), strings.Join(exportNodes(signal.Receivers), ","))
		}
	}
	for _, table := range db.ValueTables {
		fmt.Fprintf(&out, "VAL_TABLE_ %s", table.Name)
		writeDescriptions(&out, table.Values)
	}
	for _, definition := range db.AttributeDefinitions {
		scope := ""
		switch definition.Scope {
		case AttributeScopeDatabase:
		case AttributeScopeNode:
			scope = "BU_ "
		case AttributeScopeMessage:
			scope = "BO_ "
		case AttributeScopeSignal:
			scope = "SG_ "
		default:
			return nil, fmt.Errorf("attribute %s has an invalid scope", definition.Name)
		}
		fmt.Fprintf(&out, "BA_DEF_ %s%s ", scope, dbcQuote(definition.Name))
		switch definition.Kind {
		case AttributeKindInteger, AttributeKindHex, AttributeKindFloat:
			kind := []string{"INT", "HEX", "FLOAT"}[definition.Kind]
			fmt.Fprintf(&out, "%s %s %s", kind, dbcNumber(definition.Minimum), dbcNumber(definition.Maximum))
		case AttributeKindString:
			out.WriteString("STRING")
		case AttributeKindEnum:
			out.WriteString("ENUM ")
			for i, choice := range definition.Choices {
				if i != 0 {
					out.WriteByte(',')
				}
				out.WriteString(dbcQuote(choice))
			}
		default:
			return nil, fmt.Errorf("attribute %s has an invalid kind", definition.Name)
		}
		out.WriteString(";\n")
		if definition.Default != nil {
			value := exportAttribute(*definition.Default, true)
			fmt.Fprintf(&out, "BA_DEF_DEF_ %s %s;\n", dbcQuote(definition.Name), value)
		}
	}
	writeAttributes(&out, "", db.Attributes)
	for _, node := range db.Nodes {
		writeAttributes(&out, "BU_ "+node.Name+" ", node.Attributes)
	}
	for _, message := range db.Messages {
		id := exportID(message)
		if len(message.Transmitters) > 1 {
			fmt.Fprintf(&out, "BO_TX_BU_ %d : %s;\n", id, strings.Join(message.Transmitters, ","))
		}
		writeAttributes(&out, fmt.Sprintf("BO_ %d ", id), message.Attributes)
		for _, group := range message.SignalGroups {
			fmt.Fprintf(&out, "SIG_GROUP_ %d %s %d : %s;\n", id, group.Name, group.Repetitions, strings.Join(group.Signals, " "))
		}
		for _, signal := range message.Signals {
			if len(signal.Values) != 0 {
				fmt.Fprintf(&out, "VAL_ %d %s", id, signal.Name)
				writeDescriptions(&out, signal.Values)
			}
			if signal.ValueType != ValueTypeInteger {
				fmt.Fprintf(&out, "SIG_VALTYPE_ %d %s : %d;\n", id, signal.Name, signal.ValueType)
			}
			if signal.Multiplex != nil {
				fmt.Fprintf(&out, "SG_MUL_VAL_ %d %s %s ", id, signal.Name, signal.Multiplex.Selector)
				for i, interval := range signal.Multiplex.Ranges {
					if i != 0 {
						out.WriteByte(',')
					}
					fmt.Fprintf(&out, "%d-%d", interval.First, interval.Last)
				}
				out.WriteString(";\n")
			}
			writeAttributes(&out, fmt.Sprintf("SG_ %d %s ", id, signal.Name), signal.Attributes)
		}
	}
	// Reuse the parser's semantic validation instead of maintaining a second
	// validator. Also compare public fields: successful parsing alone could
	// silently normalise or drop part of an edited model.
	result := out.String()
	parsed, err := Parse("DBC export", result)
	if err != nil {
		return nil, fmt.Errorf("cannot export DBC: %w", err)
	}
	if !sameExportedModel(reflect.ValueOf(db).Elem(), reflect.ValueOf(parsed).Elem()) {
		return nil, fmt.Errorf("cannot export DBC without changing represented semantics; check names, values and frame-format attributes")
	}
	return []byte(result), nil
}

func exportID(message Message) uint32 {
	id := message.ID
	if message.Extended {
		id |= 1 << 31
	}
	return id
}

func exportNodes(nodes []string) []string {
	if len(nodes) == 0 {
		return []string{"Vector__XXX"}
	}
	return nodes
}

func dbcQuote(value string) string {
	// DBC readers consistently unescape quotes, but not C-style \n, \t or
	// doubled backslashes. Keep literal whitespace and backslashes; the model
	// comparison rejects ambiguous escapes that our parser would reinterpret.
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

func exportNames(db *Database) error {
	var names []string
	for _, node := range db.Nodes {
		names = append(names, node.Name)
	}
	for _, table := range db.ValueTables {
		names = append(names, table.Name)
	}
	for _, message := range db.Messages {
		names = append(names, message.Name)
		names = append(names, message.Transmitters...)
		for _, signal := range message.Signals {
			names = append(names, signal.Name)
			names = append(names, signal.Receivers...)
			if signal.Multiplex != nil {
				names = append(names, signal.Multiplex.Selector)
			}
		}
		for _, group := range message.SignalGroups {
			names = append(names, group.Name)
			names = append(names, group.Signals...)
		}
	}
	for _, name := range names {
		if len(name) == 0 || len(name) > 32 {
			return fmt.Errorf("DBC identifier must contain 1 to 32 ASCII characters: %q", name)
		}
		for i, c := range []byte(name) {
			if c != '_' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(i > 0 && c >= '0' && c <= '9') {
				return fmt.Errorf("invalid DBC identifier %q", name)
			}
		}
	}
	return nil
}

func dbcNumber(value float64) string { return strconv.FormatFloat(value, 'g', -1, 64) }

func writeDescriptions(out *strings.Builder, values []ValueDescription) {
	for _, value := range values {
		fmt.Fprintf(out, " %d %s", value.Value, dbcQuote(value.Label))
	}
	out.WriteString(";\n")
}

func exportAttribute(value AttributeValue, defaultValue bool) string {
	switch value.Kind {
	case AttributeKindString:
		return dbcQuote(value.Text)
	case AttributeKindEnum:
		if (defaultValue && value.Text != "") || (value.Integer == -1 && value.Text != "") {
			return dbcQuote(value.Text)
		}
	case AttributeKindFloat:
		text := dbcNumber(value.Float)
		// Undeclared attributes are inferred from their literal syntax.
		if !strings.ContainsAny(text, ".eE") {
			text += ".0"
		}
		return text
	}
	return strconv.FormatInt(value.Integer, 10)
}

func writeAttributes(out *strings.Builder, target string, attributes map[string]AttributeValue) {
	for _, name := range slices.Sorted(maps.Keys(attributes)) {
		fmt.Fprintf(out, "BA_ %s %s%s;\n", dbcQuote(name), target, exportAttribute(attributes[name], false))
	}
}

// Compare only the serialisable model, excluding diagnostics and derived
// lookup/codec state. Empty containers and DBC's absent-node placeholder are
// semantically equivalent. This also catches new public fields the exporter
// does not yet know how to preserve.
func sameExportedModel(a, b reflect.Value) bool {
	switch a.Kind() {
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			field := a.Type().Field(i)
			if !field.IsExported() || field.Name == "Diagnostics" {
				continue
			}
			if field.Name == "Transmitters" || field.Name == "Receivers" {
				if !slices.Equal(exportNodes(a.Field(i).Interface().([]string)), exportNodes(b.Field(i).Interface().([]string))) {
					return false
				}
			} else if !sameExportedModel(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Map:
		if a.Len() != b.Len() {
			return false
		}
		if a.Kind() == reflect.Slice {
			for i := 0; i < a.Len(); i++ {
				if !sameExportedModel(a.Index(i), b.Index(i)) {
					return false
				}
			}
		} else {
			for _, key := range a.MapKeys() {
				other := b.MapIndex(key)
				if !sameExportedModel(key, key) || !other.IsValid() || !sameExportedModel(a.MapIndex(key), other) {
					return false
				}
			}
		}
		return true
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return sameExportedModel(a.Elem(), b.Elem())
	case reflect.String:
		return !strings.ContainsRune(a.String(), 0) && a.String() == b.String()
	default:
		return reflect.DeepEqual(a.Interface(), b.Interface())
	}
}
