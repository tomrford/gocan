// Package asc writes Vector ASCII CAN trace files.
package asc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/tomrford/gocan"
)

var errWriterClosed = errors.New("ASC writer is closed")

const hexDigits = "0123456789ABCDEF"

// Writer writes frames and events to an ASC stream.
//
// Writer buffers output and is not safe for concurrent use. Close writes the
// ASC footer and flushes buffered data, but does not close the underlying
// io.Writer. The header is written when the first record supplies the
// measurement start time.
//
// The header date is written in local time. ASC has no time zone field, and
// Vector tools read the header as local time. Timestamps must not decrease;
// a regressing record is rejected without being written. Equal times are allowed.
type Writer struct {
	output *bufio.Writer

	started bool
	closed  bool
	start   time.Time
	last    time.Time

	// scratch holds the record body under construction between writes.
	scratch []byte
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

	buf := writer.scratch[:0]
	if event.Frame.Flags.Has(gocan.FrameFD) {
		buf = append(buf, "CANFD "...)
		buf = strconv.AppendInt(buf, int64(event.Bus), 10)
		buf = append(buf, ' ')
		buf = appendDirection(buf, event.Direction)
		buf = append(buf, ' ')
		buf = appendHexUpper(buf, event.Frame.ID)
		if event.Frame.Flags.Has(gocan.FrameExtended) {
			buf = append(buf, 'x')
		}
		buf = append(buf, " - "...)
		buf = append(buf, boolDigit(event.Frame.Flags.Has(gocan.FrameBitRateSwitch))+'0')
		buf = append(buf, ' ')
		buf = append(buf, boolDigit(event.Frame.Flags.Has(gocan.FrameErrorStateIndicator))+'0')
		buf = append(buf, ' ')
		buf = strconv.AppendUint(buf, uint64(event.Frame.DLC), 16)
		buf = append(buf, ' ')
		buf = strconv.AppendInt(buf, int64(event.Frame.DataLength()), 10)
		buf = appendData(buf, event.Frame)

		flags := uint32(1 << 12)
		if event.Frame.Flags.Has(gocan.FrameBitRateSwitch) {
			flags |= 1 << 13
		}
		if event.Frame.Flags.Has(gocan.FrameErrorStateIndicator) {
			flags |= 1 << 14
		}
		buf = append(buf, " 0 0 "...)
		buf = appendHexUpper(buf, flags)
		buf = append(buf, " 0 0 0 0 0"...)
	} else if event.Frame.Flags.Has(gocan.FrameRemote) {
		buf = strconv.AppendInt(buf, int64(event.Bus), 10)
		buf = append(buf, ' ')
		buf = appendHexUpper(buf, event.Frame.ID)
		if event.Frame.Flags.Has(gocan.FrameExtended) {
			buf = append(buf, 'x')
		}
		buf = append(buf, ' ')
		buf = appendDirection(buf, event.Direction)
		buf = append(buf, " r "...)
		buf = strconv.AppendUint(buf, uint64(event.Frame.DLC), 16)
	} else {
		buf = strconv.AppendInt(buf, int64(event.Bus), 10)
		buf = append(buf, ' ')
		buf = appendHexUpper(buf, event.Frame.ID)
		if event.Frame.Flags.Has(gocan.FrameExtended) {
			buf = append(buf, 'x')
		}
		buf = append(buf, ' ')
		buf = appendDirection(buf, event.Direction)
		buf = append(buf, " d "...)
		buf = strconv.AppendUint(buf, uint64(event.Frame.DLC), 16)
		buf = appendData(buf, event.Frame)
	}
	writer.scratch = buf

	return writer.writeRecord(event.Timestamp)
}

// WriteEvent writes one non-frame Capture event. ASC has native forms for
// CAN error frames and controller state. Receive overrun is written as a
// timestamped internal status event.
func (writer *Writer) WriteEvent(event gocan.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}

	buf := writer.scratch[:0]
	switch event.Kind {
	case gocan.EventControllerState:
		buf = append(buf, "CAN "...)
		buf = strconv.AppendInt(buf, int64(event.Bus), 10)
		buf = append(buf, " Status:chip status "...)
		buf = append(buf, controllerState(event.ControllerState)...)
		if event.ErrorCountsKnown {
			buf = append(buf, " - TxErr: "...)
			buf = strconv.AppendInt(buf, int64(event.TXErrorCount), 10)
			buf = append(buf, " RxErr: "...)
			buf = strconv.AppendInt(buf, int64(event.RXErrorCount), 10)
		}
	case gocan.EventErrorFrame:
		buf = strconv.AppendInt(buf, int64(event.Bus), 10)
		buf = append(buf, " ErrorFrame"...)
	case gocan.EventReceiveOverrun:
		buf = append(buf, "CAN "...)
		buf = strconv.AppendInt(buf, int64(event.Bus), 10)
		buf = append(buf, " Status:receive queue overrun"...)
	default:
		return fmt.Errorf("unsupported gocan event kind %d", event.Kind)
	}
	writer.scratch = buf

	return writer.writeRecord(event.Timestamp)
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

// writeRecord emits the frozen body in scratch behind the timestamp prefix.
func (writer *Writer) writeRecord(timestamp time.Time) error {
	if writer.closed {
		return errWriterClosed
	}
	if !writer.started {
		if err := writer.writeHeader(timestamp); err != nil {
			return err
		}
	}
	if timestamp.Before(writer.last) {
		return fmt.Errorf("ASC timestamp %s precedes previous record %s", timestamp, writer.last)
	}

	offset := timestamp.Sub(writer.start).Microseconds()
	// The ASC timestamp field is "%9d.%06d": seconds right-aligned in nine
	// characters, microseconds zero-padded to six.
	prefix := appendPadded(nil, offset/1_000_000, 9, ' ')
	prefix = append(prefix, '.')
	prefix = appendPadded(prefix, offset%1_000_000, 6, '0')
	prefix = append(prefix, ' ')

	if _, err := writer.output.Write(prefix); err != nil {
		return err
	}
	if _, err := writer.output.Write(writer.scratch); err != nil {
		return err
	}
	if err := writer.output.WriteByte('\n'); err != nil {
		return err
	}
	writer.last = timestamp
	return nil
}

func (writer *Writer) writeHeader(timestamp time.Time) error {
	formatted := timestamp.In(time.Local).Format("Mon Jan 02 15:04:05.000 2006")
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

func appendData(dst []byte, frame gocan.Frame) []byte {
	for _, value := range frame.Data[:frame.DataLength()] {
		dst = append(dst, ' ', hexDigits[value>>4], hexDigits[value&0x0f])
	}
	return dst
}

func appendHexUpper(dst []byte, value uint32) []byte {
	if value == 0 {
		return append(dst, '0')
	}
	var digits [8]byte
	count := 0
	for value > 0 {
		digits[count] = hexDigits[value&0xf]
		value >>= 4
		count++
	}
	for i := count - 1; i >= 0; i-- {
		dst = append(dst, digits[i])
	}
	return dst
}

// appendPadded appends value right-aligned in width characters, filling the
// gap with fill.
func appendPadded(dst []byte, value int64, width int, fill byte) []byte {
	start := len(dst)
	dst = strconv.AppendInt(dst, value, 10)
	if padding := width - (len(dst) - start); padding > 0 {
		dst = append(dst, make([]byte, padding)...)
		copy(dst[start+padding:], dst[start:])
		for i := 0; i < padding; i++ {
			dst[start+i] = fill
		}
	}
	return dst
}

func appendDirection(dst []byte, direction gocan.Direction) []byte {
	if direction == gocan.DirectionTransmit {
		return append(dst, "Tx"...)
	}
	return append(dst, "Rx"...)
}

func boolDigit(value bool) byte {
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
