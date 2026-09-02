package dbc

import (
	"errors"
	"os"
	"testing"
)

func TestParseCoreDatabase(t *testing.T) {
	db := parseFixture(t, "testdata/core.dbc")

	if db.Version != "1.0" || len(db.Nodes) != 3 || len(db.Messages) != 2 {
		t.Fatalf("unexpected database header: version=%q nodes=%d messages=%d", db.Version, len(db.Nodes), len(db.Messages))
	}

	status := db.Messages[0]
	if status.ID != 0x100 || status.Extended || status.Format != FrameFormatStandardCAN {
		t.Fatalf("unexpected Status identity: %#v", status)
	}
	if status.Attributes["GenMsgCycleTime"].Integer != 10 || status.Attributes["GenMsgSendType"].Text != "Cyclic" {
		t.Fatalf("unexpected Status scheduling attributes: %#v", status.Attributes)
	}
	if len(status.Transmitters) != 2 || status.Transmitters[0] != "ECU" || status.Transmitters[1] != "Logger" {
		t.Fatalf("unexpected Status transmitters: %v", status.Transmitters)
	}
	if len(status.Signals) != 4 || status.Signals[2].ByteOrder != ByteOrderBigEndian {
		t.Fatalf("unexpected Status signals: %#v", status.Signals)
	}
	ratio, ok := status.SignalByName("Ratio")
	if !ok {
		t.Fatal("Ratio signal was not resolved")
	}
	if ratio.ValueType != ValueTypeFloat32 {
		t.Fatalf("Ratio value type = %v, want float32", ratio.ValueType)
	}
	temperature, ok := status.SignalByName("Temperature")
	if !ok {
		t.Fatal("Temperature signal was not resolved")
	}
	if temperature.Attributes["SPN"].Integer != 171 {
		t.Fatalf("Temperature SPN = %v, want 171", temperature.Attributes["SPN"])
	}
	counter, ok := status.SignalByName("Counter")
	if !ok {
		t.Fatal("Counter signal was not resolved")
	}
	if len(counter.Values) != 2 || counter.Values[1].Label != "Running" {
		t.Fatalf("unexpected Counter choices: %v", counter.Values)
	}
	if _, ok := status.SignalByName("Missing"); ok {
		t.Fatal("unknown signal was resolved")
	}
	if len(status.SignalGroups) != 1 || len(status.SignalGroups[0].Signals) != 2 {
		t.Fatalf("unexpected signal groups: %v", status.SignalGroups)
	}

	fd := db.Messages[1]
	if fd.ID != 0x1abcde || !fd.Extended || fd.Format != FrameFormatExtendedCANFD {
		t.Fatalf("unexpected FastStatus metadata: %#v", fd)
	}
	if value := db.Attributes["BusType"]; value.Text != "CAN FD" {
		t.Fatalf("BusType = %#v, want CAN FD", value)
	}
}

func TestParseMultiplexingAndJ1939(t *testing.T) {
	db := parseFixture(t, "testdata/multiplex_j1939.dbc")

	mux := db.Messages[0]
	assertMux := func(name, selector string, first, last uint64) {
		t.Helper()
		signal, ok := mux.SignalByName(name)
		if !ok {
			t.Fatalf("signal %q was not resolved", name)
		}
		condition := signal.Multiplex
		if condition == nil || condition.Selector != selector || len(condition.Ranges) != 1 ||
			condition.Ranges[0] != (MultiplexRange{First: first, Last: last}) {
			t.Fatalf("unexpected multiplex condition for %s: %#v", name, condition)
		}
	}
	rootA, ok := mux.SignalByName("RootA")
	if !ok {
		t.Fatal("RootA signal was not resolved")
	}
	if !rootA.IsMultiplexer || rootA.Multiplex != nil {
		t.Fatalf("RootA is not an unconditional multiplexer: %#v", rootA)
	}
	childSelector, ok := mux.SignalByName("ChildSelector")
	if !ok {
		t.Fatal("ChildSelector signal was not resolved")
	}
	if !childSelector.IsMultiplexer {
		t.Fatalf("ChildSelector is not a multiplexer: %#v", childSelector)
	}
	assertMux("ChildSelector", "RootA", 2, 2)
	assertMux("Leaf", "ChildSelector", 3, 5)
	assertMux("Other", "RootB", 7, 9)

	j1939 := db.Messages[1]
	if j1939.Format != FrameFormatJ1939 || j1939.ID != 0x18feee80 || !j1939.Extended {
		t.Fatalf("message was not resolved as J1939: %#v", j1939)
	}
	coolant, ok := j1939.SignalByName("Coolant")
	if !ok {
		t.Fatal("Coolant signal was not resolved")
	}
	if coolant.Attributes["SPN"].Integer != 110 {
		t.Fatalf("Coolant SPN = %v, want 110", coolant.Attributes["SPN"])
	}
}

func TestDropsDanglingRecordsWithDiagnostics(t *testing.T) {
	source := "BU_: ECU\n" +
		"BO_ 3221225472 VECTOR__INDEPENDENT_SIG_MSG: 0 Vector__XXX\n" +
		" SG_ Orphan : 0|8@1+ (1,0) [0|0] \"\" Vector__XXX\n" +
		"BO_ 256 Status: 8 ECU\n" +
		" SG_ Counter : 0|8@1+ (1,0) [0|255] \"\" ECU\n" +
		"CM_ SG_ 512 Missing \"stale comment\";\n" +
		"VAL_ 3221225472 Orphan 0 \"Off\";\n" +
		"BO_TX_BU_ 512 : ECU;\n"

	db, err := Parse("dangling.dbc", source)
	if err != nil {
		t.Fatal(err)
	}
	if len(db.Messages) != 1 || db.Messages[0].Name != "Status" {
		t.Fatalf("unexpected messages: %#v", db.Messages)
	}
	// The pseudo-message and BO_TX_BU_ each report once. Comments are discarded,
	// and the VAL_ that targets the dropped pseudo-message is skipped silently.
	if len(db.Diagnostics) != 2 {
		t.Fatalf("unexpected diagnostics: %#v", db.Diagnostics)
	}
}

func TestRejectsStructurallyUnsafeDatabases(t *testing.T) {
	tests := map[string]string{
		"orphan signal":        "SG_ Value : 0|8@1+ (1,0) [0|1] \"\" ECU\n",
		"legacy signal type":   "SGTYPE_ Old : 8@1+ (1,0) [0|1] \"\" 0;\n",
		"oversize CAN message": "BO_ 1 Oversize: 65 ECU\n",
		"ambiguous simple multiplexing": "BU_: ECU\n" +
			"BO_ 1 Ambiguous: 8 ECU\n" +
			" SG_ A M : 0|8@1+ (1,0) [0|0] \"\" ECU\n" +
			" SG_ B M : 8|8@1+ (1,0) [0|0] \"\" ECU\n" +
			" SG_ Value m1 : 16|8@1+ (1,0) [0|0] \"\" ECU\n",
	}

	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(name+".dbc", source)
			if err == nil {
				t.Fatal("Parse() succeeded, want an error")
			}
			var parseErr *Error
			if !errors.As(err, &parseErr) || parseErr.Position.Line == 0 {
				t.Fatalf("Parse() error lacks source position: %v", err)
			}
		})
	}
}

func parseFixture(t *testing.T, path string) *Database {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Parse(path, string(source))
	if err != nil {
		t.Fatal(err)
	}
	return db
}
