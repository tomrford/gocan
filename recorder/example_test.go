package recorder_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/asc"
	"github.com/tomrford/gocan/recorder"
)

func ExampleStart() {
	capture := gocan.NewCapture()
	frame, err := gocan.NewFrame(0x200, []byte{0xaa, 0xbb}, 0)
	if err != nil {
		panic(err)
	}
	if err := capture.Append(gocan.FrameEvent{
		Bus:       1,
		Timestamp: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		Direction: gocan.DirectionReceive,
		Frame:     frame,
	}); err != nil {
		panic(err)
	}

	var buf bytes.Buffer
	rec, err := recorder.Start(context.Background(), capture, asc.NewWriter(&buf), gocan.Cursor{}, time.Second)
	if err != nil {
		panic(err)
	}
	if err := rec.Close(); err != nil {
		panic(err)
	}

	fmt.Println(strings.Contains(buf.String(), "200 Rx d 2 AA BB"))
	// Output:
	// true
}
