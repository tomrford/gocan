//go:build windows && amd64

package nixnet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/internal/driverstate"
	"golang.org/x/sys/windows"
)

type Bus struct {
	config     Config
	capture    *gocan.Capture
	api        *api
	rx, tx     uint32
	ioMu       sync.Mutex
	lifecycle  *driverstate.Lifecycle
	cleanupErr error
	lastState  gocan.Event
}

var _ gocan.Bus = (*Bus)(nil)

func Open(ctx context.Context, capture *gocan.Capture, config Config) (_ *Bus, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, err := loadAPI()
	if err != nil {
		return nil, err
	}
	bus := &Bus{config: config, capture: capture, api: a, lifecycle: driverstate.New(nil)}
	defer func() {
		if err != nil {
			err = errors.Join(err, bus.cleanup())
		}
	}()
	database := ":memory:"
	if config.FD {
		database = ":can_fd_brs:"
	}
	db, _ := windows.BytePtrFromString(database)
	empty, _ := windows.BytePtrFromString("")
	intf, e := windows.BytePtrFromString(config.Interface)
	if e != nil {
		return nil, e
	}
	for _, session := range []struct {
		ref  *uint32
		mode uintptr
	}{{&bus.rx, 6}, {&bus.tx, 9}} {
		result, _, _ := a.create.Call(uintptr(unsafe.Pointer(db)), uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(intf)), session.mode, uintptr(unsafe.Pointer(session.ref)))
		if err = a.check("create NI-XNET stream", result); err != nil {
			return nil, err
		}
	}
	if err = a.set64(bus.tx, propBaud, config.Baud); err != nil {
		return nil, err
	}
	if config.FD {
		if err = a.set64(bus.tx, propFDBaud, config.FDBaud); err != nil {
			return nil, err
		}
		if err = a.set32(bus.tx, propISO, 0); err != nil {
			return nil, err
		}
	}
	for _, property := range []struct {
		id    uintptr
		value uint32
	}{{propEcho, 0}, {propErrors, 1}, {propTiming, 0}, {propPendingOrder, 0}} {
		if err = a.set32(bus.tx, property.id, property.value); err != nil {
			return nil, err
		}
	}
	if config.Termination != 0 {
		if err = a.set32(bus.tx, propTermination, uint32(config.Termination-1)); err != nil {
			return nil, err
		}
	}
	// Start input first so Open returns ready to capture the first transmission.
	for _, ref := range []uint32{bus.rx, bus.tx} {
		result, _, _ := a.start.Call(uintptr(ref), 0)
		if err = a.check("start NI-XNET stream", result); err != nil {
			return nil, err
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	go bus.receiveLoop()
	return bus, nil
}

func (bus *Bus) ID() gocan.BusID         { return bus.config.ID }
func (bus *Bus) Name() string            { return bus.config.Name }
func (bus *Bus) Capture() *gocan.Capture { return bus.capture }
func (bus *Bus) Done() <-chan struct{}   { return bus.lifecycle.Done() }
func (bus *Bus) Err() error              { return bus.lifecycle.Err() }
func (bus *Bus) Close() error            { bus.lifecycle.Stop(nil); <-bus.Done(); return bus.cleanupErr }

func (bus *Bus) Send(ctx context.Context, frame gocan.Frame) error {
	data, size, err := encodeFrame(frame, bus.config.FD)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bus.ioMu.Lock()
	defer bus.ioMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-bus.lifecycle.StopSignal():
		return bus.lifecycle.OperationError()
	default:
	}
	// On windows/amd64 Proc.Call copies argument 4 into XMM3 as well as R9.
	// Timeout 0 is the all-zero float64 representation: never wait for TX space.
	result, _, _ := bus.api.write.Call(uintptr(bus.tx), uintptr(unsafe.Pointer(&data[0])), uintptr(size), 0)
	if err := bus.api.writeError(result); err != nil {
		if !errors.Is(err, gocan.ErrTransmitQueueFull) {
			bus.lifecycle.Stop(err)
		}
		return err
	}
	if err := bus.capture.RecordFrame(gocan.FrameEvent{Bus: bus.ID(), Direction: gocan.DirectionTransmit, Frame: frame}); err != nil {
		bus.lifecycle.Stop(err)
		return err
	}
	return nil
}

func (bus *Bus) receiveLoop() {
	defer func() { bus.ioMu.Lock(); bus.cleanupErr = bus.cleanup(); bus.ioMu.Unlock(); bus.lifecycle.MarkDone() }()
	idle := time.NewTimer(time.Hour)
	if !idle.Stop() {
		<-idle.C
	}
	defer idle.Stop()
	nextState := time.Now()
	for {
		select {
		case <-bus.lifecycle.StopSignal():
			return
		default:
		}
		bus.ioMu.Lock()
		more, err := bus.receiveOne()
		if err == nil && !time.Now().Before(nextState) {
			err = bus.checkState()
			nextState = time.Now().Add(100 * time.Millisecond)
		}
		bus.ioMu.Unlock()
		if err != nil {
			bus.lifecycle.Stop(err)
			return
		}
		if more {
			continue
		}
		idle.Reset(time.Millisecond)
		select {
		case <-bus.lifecycle.StopSignal():
			return
		case <-idle.C:
		}
	}
}

// readOne uses the smallest buffer that can hold a frame in this mode. The
// variable-width FD ABI can also return several shorter complete records.
func (bus *Bus) receiveOne() (bool, error) {
	var data [frameMaxSize]byte
	size := frameClassicSize
	if bus.config.FD {
		size = frameMaxSize
	}
	var returned uint32
	result, _, _ := bus.api.read.Call(uintptr(bus.rx), uintptr(unsafe.Pointer(&data[0])), uintptr(size), 0, uintptr(unsafe.Pointer(&returned)))
	if err := bus.api.check("read NI-XNET frame", result); err != nil {
		if errors.Is(err, gocan.ErrReceiveOverrun) {
			_ = bus.capture.RecordEvent(gocan.Event{Bus: bus.ID(), Kind: gocan.EventReceiveOverrun})
		}
		return false, err
	}
	if returned > uint32(size) {
		return false, errors.New("NI-XNET returned more data than requested")
	}
	for offset := 0; offset < int(returned); {
		record := data[offset:returned]
		if len(record) < frameClassicSize {
			return false, errors.New("NI-XNET returned a truncated record")
		}
		length := recordSize(int(record[15]))
		if length > len(record) {
			return false, errors.New("NI-XNET returned a truncated payload")
		}
		if record[13]&echoFlag != 0 {
			return false, errors.New("NI-XNET returned an unexpected transmit echo")
		}
		if record[12] == frameError {
			if record[15] < 5 {
				return false, errors.New("NI-XNET returned a short bus error")
			}
			if err := bus.capture.RecordEvent(gocan.Event{Bus: bus.ID(), Kind: gocan.EventErrorFrame}); err != nil {
				return false, err
			}
			if err := bus.recordState(record[16], record[17], record[18]); err != nil {
				return false, err
			}
		} else {
			frame, err := decodeFrame(record[:length])
			if err != nil {
				return false, err
			}
			if err := bus.capture.RecordFrame(gocan.FrameEvent{Bus: bus.ID(), Direction: gocan.DirectionReceive, Frame: frame}); err != nil {
				return false, err
			}
		}
		offset += length
	}
	return returned != 0, nil
}

func (bus *Bus) checkState() error {
	var state, fault uint32
	result, _, _ := bus.api.state.Call(uintptr(bus.rx), stateCAN, 4, uintptr(unsafe.Pointer(&state)), uintptr(unsafe.Pointer(&fault)))
	if err := bus.api.check("read NI-XNET controller state", result); err != nil {
		return err
	}
	if err := bus.api.check("NI-XNET controller fault", uintptr(fault)); err != nil {
		return err
	}
	// INIT is expected while integrating into an idle bus; it is not bus-off.
	if state&15 == 3 {
		return nil
	}
	return bus.recordState(byte(state&15), byte(state>>16), byte(state>>24))
}
func (bus *Bus) recordState(state, tx, rx byte) error {
	event, err := controllerEvent(bus.ID(), state, tx, rx)
	if err != nil {
		return err
	}
	if event != bus.lastState {
		if err := bus.capture.RecordEvent(event); err != nil {
			return err
		}
		bus.lastState = event
	}
	if state == 2 {
		return fmt.Errorf("%w: NI-XNET controller", gocan.ErrBusOff)
	}
	return nil
}
func (bus *Bus) cleanup() error {
	var errs []error
	for _, ref := range []uint32{bus.tx, bus.rx} {
		if ref == 0 {
			continue
		}
		result, _, _ := bus.api.clear.Call(uintptr(ref))
		errs = append(errs, bus.api.check("clear NI-XNET stream", result))
	}
	return errors.Join(errs...)
}
