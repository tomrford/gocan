package asc_test

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/asc"
)

func ExampleWriter() {
	var buf bytes.Buffer
	writer := asc.NewWriter(&buf)

	frame, err := gocan.NewFrame(0x123, []byte{0x01, 0x02, 0x03}, 0)
	if err != nil {
		panic(err)
	}
	if err := writer.WriteFrame(gocan.FrameEvent{
		Bus:       1,
		Timestamp: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		Direction: gocan.DirectionReceive,
		Frame:     frame,
	}); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}

	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "123") {
			fmt.Println(strings.TrimSpace(line))
		}
	}
	// Output:
	// 0.000000 1 123 Rx d 3 01 02 03
}
