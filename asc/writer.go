// Package asc writes Vector ASCII CAN trace files.
package asc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tomrford/gocan"
)

// TODO: Export this sentinel (compare net.ErrClosed) when a caller needs to
// detect writes after Close with errors.Is.
var errWriterClosed = errors.New("ASC writer is closed")

// Writer writes frames and events to an ASC stream.
//
// Writer buffers output and is not safe for concurrent use. Close writes the
// ASC footer and flushes buffered data, but does not close the underlying
// io.Writer. The header is written when the first record supplies the
// measurement start time.
//
// The header date is written in UTC; ASC has no time zone field, so Vector
// tools read it as local time. A record whose timestamp regresses is written
// at the previous record's time, so offsets never decrease.
type Writer struct {
	output *bufio.Writer

	started bool
	closed  bool
	start   time.Time
	last    time.Time
}

var _ gocan.RecordWriter = (*Writer)(nil)

// NewWriter returns a Writer that writes to output.
func NewWriter(output io.Writer) *Writer {
	return &Writer{output: bufio.NewWriter(output)}
}

// WriteFrame writes one classical CAN or CAN FD frame.
func (writer *Writer) WriteFrame(event gocan.FrameEvent) error {
	if err := event.Validate(); err != nil {
		return err
	}

	var body strings.Builder
	id := fmt.Sprintf("%X", event.Frame.ID)
	if event.Frame.Flags.Has(gocan.FrameExtended) {
		id += "x"
	}
	direction := "Rx"
	if event.Direction == gocan.DirectionTransmit {
		direction = "Tx"
	}

	if event.Frame.Flags.Has(gocan.FrameFD) {
		flags := uint32(1 << 12)
		if event.Frame.Flags.Has(gocan.FrameBitRateSwitch) {
			flags |= 1 << 13
		}
		if event.Frame.Flags.Has(gocan.FrameErrorStateIndicator) {
			flags |= 1 << 14
		}
		fmt.Fprintf(
			&body,
			"CANFD %d %s %s - %d %d %x %d",
			event.Bus,
			direction,
			id,
			boolDigit(event.Frame.Flags.Has(gocan.FrameBitRateSwitch)),
			boolDigit(event.Frame.Flags.Has(gocan.FrameErrorStateIndicator)),
			event.Frame.DLC,
			event.Frame.DataLength(),
		)
		writeData(&body, event.Frame)
		fmt.Fprintf(&body, " 0 0 %X 0 0 0 0 0", flags)
	} else if event.Frame.Flags.Has(gocan.FrameRemote) {
		fmt.Fprintf(&body, "%d %s %s r %x", event.Bus, id, direction, event.Frame.DLC)
	} else {
		fmt.Fprintf(&body, "%d %s %s d %x", event.Bus, id, direction, event.Frame.DLC)
		writeData(&body, event.Frame)
	}

	return writer.writeRecord(event.Timestamp, body.String())
}

// WriteEvent writes one non-frame Capture event. ASC has native forms for
// CAN error frames and controller state. Receive overrun is written as a
// timestamped internal status event.
func (writer *Writer) WriteEvent(event gocan.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}

	var body string
	switch event.Kind {
	case gocan.EventControllerState:
		body = fmt.Sprintf("CAN %d Status:chip status %s", event.Bus, controllerState(event.ControllerState))
		if event.ErrorCountsKnown {
			body += fmt.Sprintf(" - TxErr: %d RxErr: %d", event.TXErrorCount, event.RXErrorCount)
		}
	case gocan.EventErrorFrame:
		body = fmt.Sprintf("%d ErrorFrame", event.Bus)
	case gocan.EventReceiveOverrun:
		// TODO: Replace this readable internal status only if hardware tools
		// establish a better interoperable ASC form for receive loss.
		body = fmt.Sprintf("CAN %d Status:receive queue overrun", event.Bus)
	default:
		return fmt.Errorf("unsupported gocan event kind %d", event.Kind)
	}
	return writer.writeRecord(event.Timestamp, body)
}

// Flush writes buffered data to the underlying writer.
func (writer *Writer) Flush() error {
	return writer.output.Flush()
}

// Close writes the ASC footer and flushes buffered data. It does not close the
// underlying writer. Close is safe to call more than once.
func (writer *Writer) Close() error {
	if writer.closed {
		return nil
	}
	writer.closed = true
	if writer.started {
		if _, err := writer.output.WriteString("End TriggerBlock\n"); err != nil {
			return err
		}
	}
	return writer.output.Flush()
}

func (writer *Writer) writeRecord(timestamp time.Time, body string) error {
	if writer.closed {
		return errWriterClosed
	}
	if !writer.started {
		if err := writer.writeHeader(timestamp); err != nil {
			return err
		}
	}
	if timestamp.Before(writer.last) {
		timestamp = writer.last
	}

	offset := timestamp.Sub(writer.start).Microseconds()
	seconds := offset / 1_000_000
	microseconds := offset % 1_000_000
	if _, err := fmt.Fprintf(writer.output, "%9d.%06d %s\n", seconds, microseconds, body); err != nil {
		return err
	}
	writer.last = timestamp
	return nil
}

func (writer *Writer) writeHeader(timestamp time.Time) error {
	formatted := timestamp.UTC().Format("Mon Jan 02 15:04:05.000 2006")
	if _, err := fmt.Fprintf(
		writer.output,
		"date %s\nbase hex timestamps absolute\ninternal events logged\nBegin Triggerblock %s\n 0.000000 Start of measurement\n",
		formatted,
		formatted,
	); err != nil {
		return err
	}
	writer.started = true
	writer.start = timestamp
	writer.last = timestamp
	return nil
}

func writeData(output *strings.Builder, frame gocan.Frame) {
	for _, value := range frame.Data[:frame.DataLength()] {
		fmt.Fprintf(output, " %02X", value)
	}
}

func boolDigit(value bool) int {
	if value {
		return 1
	}
	return 0
}

func controllerState(state gocan.ControllerState) string {
	switch state {
	case gocan.ControllerActive:
		return "error active"
	case gocan.ControllerWarning:
		return "error warning"
	case gocan.ControllerPassive:
		return "error passive"
	case gocan.ControllerBusOff:
		return "bus off"
	default:
		panic("gocan: validated controller state is invalid")
	}
}
