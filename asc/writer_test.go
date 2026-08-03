package asc_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/asc"
)

func TestWriterStreamsCapture(t *testing.T) {
	start := time.Date(2026, time.August, 1, 12, 34, 56, 789_000_000, time.UTC)
	capture := gocan.NewCapture()

	classic, err := gocan.NewFrame(0x123, []byte{0xaa, 0xbb}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.Append(gocan.FrameEvent{
		Bus: 1, Timestamp: start, Direction: gocan.DirectionReceive, Frame: classic,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := gocan.NewControllerStateEvent(
		1, start.Add(time.Millisecond), gocan.ControllerPassive, 132, 0, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.AppendEvent(state); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writer := asc.NewWriter(&output)
	cursor, err := capture.WriteRecordsSince(gocan.Cursor{}, writer)
	if err != nil {
		t.Fatal(err)
	}

	remote, err := gocan.NewRemoteFrame(0x321, 8, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.Append(gocan.FrameEvent{
		Bus: 1, Timestamp: start.Add(2 * time.Millisecond), Direction: gocan.DirectionTransmit, Frame: remote,
	}); err != nil {
		t.Fatal(err)
	}
	fd, err := gocan.NewFrame(
		0x1abcde,
		[]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
		gocan.FrameExtended|gocan.FrameFD|gocan.FrameBitRateSwitch|gocan.FrameErrorStateIndicator,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.Append(gocan.FrameEvent{
		Bus: 2, Timestamp: start.Add(3 * time.Millisecond), Direction: gocan.DirectionTransmit, Frame: fd,
	}); err != nil {
		t.Fatal(err)
	}
	errorFrame, err := gocan.NewErrorFrameEvent(2, start.Add(4*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.AppendEvent(errorFrame); err != nil {
		t.Fatal(err)
	}
	overrun, err := gocan.NewReceiveOverrunEvent(1, start.Add(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.AppendEvent(overrun); err != nil {
		t.Fatal(err)
	}
	cursor, err = capture.WriteRecordsSince(cursor, writer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capture.WriteRecordsSince(cursor, writer); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		lines = append(lines, strings.TrimSpace(line))
	}
	want := []string{
		"date Sat Aug 01 12:34:56.789 2026",
		"base hex timestamps absolute",
		"internal events logged",
		"Begin Triggerblock Sat Aug 01 12:34:56.789 2026",
		"0.000000 Start of measurement",
		"0.000000 1 123 Rx d 2 AA BB",
		"0.001000 CAN 1 Status:chip status error passive - TxErr: 132 RxErr: 0",
		"0.002000 1 321 Tx r 8",
		"0.003000 CANFD 2 Tx 1ABCDEx - 1 1 9 12 00 01 02 03 04 05 06 07 08 09 0A 0B 0 0 7000 0 0 0 0 0",
		"0.004000 2 ErrorFrame",
		"0.005000 CAN 1 Status:receive queue overrun",
		"End TriggerBlock",
	}
	if len(lines) != len(want) {
		t.Fatalf("ASC line count = %d, want %d\n%s", len(lines), len(want), output.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("ASC line %d = %q, want %q", i+1, lines[i], want[i])
		}
	}
}
