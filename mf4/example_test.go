package mf4_test

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/mf4"
)

func ExampleWriter() {
	file, err := os.CreateTemp("", "gocan-example-*.mf4")
	if err != nil {
		panic(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()

	writer, err := mf4.NewWriter(file, mf4.Config{})
	if err != nil {
		panic(err)
	}
	payload := []byte{0xde, 0xad, 0xbe, 0xef}
	frame, err := gocan.NewFrame(0x123, payload, 0)
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

	data, err := os.ReadFile(file.Name())
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s %s frame=%t\n",
		strings.TrimSpace(string(data[:8])),
		strings.TrimSpace(string(data[8:16])),
		bytes.Contains(data, []byte("CAN_DataFrame")) && bytes.Contains(data, payload),
	)
	// Output:
	// MDF 4.10 frame=true
}
