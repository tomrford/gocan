package dbc_test

import (
	"fmt"
	"strings"

	"github.com/tomrford/gocan/dbc"
)

func ExampleParse() {
	const source = `BU_: ECU
BO_ 256 Engine: 8 ECU
 SG_ Speed : 0|16@1+ (0.25,0) [0|16383.75] "rpm" ECU
`
	db, err := dbc.Parse("engine.dbc", strings.NewReader(source))
	if err != nil {
		panic(err)
	}
	msg, ok := db.MessageByName("Engine")
	if !ok {
		panic("missing Engine message")
	}
	frame, err := msg.Encode(dbc.Values{"Speed": 1500.0})
	if err != nil {
		panic(err)
	}
	speed, err := msg.Decode(frame, "Speed")
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s -> %v rpm\n", frame, speed)
	// Output:
	// 100#7017000000000000 -> 1500 rpm
}
