//go:build windows && amd64

package vector

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

const (
	xlSuccess                xlStatus     = 0
	xlQueueEmpty             xlStatus     = 10
	xlQueueFull              xlStatus     = 11
	xlInvalidHandle          xlStatus     = 155
	xlQueueOverrun           xlStatus     = 216
	xlInvalidPort            xlPortHandle = -1
	vectorApplication                     = "gocan\x00"
	chipStateRequestInterval              = 50 * time.Millisecond
	chipStateHealthInterval               = time.Second
)

var processXLDriver xlDriverProcess

// TODO: Compare this explicit open/close ownership with the XL library's
// native reference count only if NI-XNET or another multi-session driver
// proves a simpler lifecycle; the two-Vector close-order matrix passed as is.
type xlDriverProcess struct {
	mu          sync.Mutex
	users       int
	closeFailed error
}

func (driver *xlDriverProcess) acquire(open func() error) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()

	if driver.closeFailed != nil {
		return fmt.Errorf("open Vector driver after an indeterminate close failure: %w", driver.closeFailed)
	}
	if driver.users == 0 {
		if err := open(); err != nil {
			return err
		}
	}
	driver.users++
	return nil
}

func (driver *xlDriverProcess) release(close func() error) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()

	if driver.users == 0 {
		return errors.New("release Vector driver without an owner")
	}
	driver.users--
	if driver.users != 0 {
		return nil
	}
	if err := close(); err != nil {
		// The XL process state is unknowable after a failed final close. Refuse
		// later opens rather than either leaking an assumed owner or opening the
		// native driver a second time. A process restart is the recovery path.
		driver.closeFailed = err
		return err
	}
	return nil
}

// Open initializes one Vector CAN or CAN FD channel and starts capturing frames.
// Context controls opening only; canceling it after Open returns does not stop
// the bus. Native calls cannot be interrupted, so Open checks ctx between them.
func Open(ctx context.Context, capture *gocan.Capture, config Config) (openedBus *Bus, err error) {
	if err := validateConfig(capture, config); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	api, err := loadXLAPI(config.fd())
	if err != nil {
		return nil, err
	}

	driverAcquired := false
	portOpen := false
	channelActive := false
	port := xlInvalidPort
	access := channelAccess(config.ChannelIndex)
	var stopEvent windows.Handle
	defer func() {
		if openedBus != nil {
			return
		}
		var cleanupErrors []error
		if channelActive {
			result, _, _ := api.deactivateChannel.Call(portArgument(port), uintptr(access))
			if status := xlStatus(result); status != xlSuccess {
				cleanupErrors = append(cleanupErrors, api.statusError("deactivate Vector channel after failed open", status))
			}
		}
		if portOpen {
			result, _, _ := api.closePort.Call(portArgument(port))
			if status := xlStatus(result); status != xlSuccess {
				cleanupErrors = append(cleanupErrors, api.statusError("close Vector port after failed open", status))
			}
		}
		if driverAcquired {
			if cleanupErr := processXLDriver.release(api.close); cleanupErr != nil {
				cleanupErrors = append(cleanupErrors, cleanupErr)
			}
		}
		if stopEvent != 0 {
			if cleanupErr := windows.CloseHandle(stopEvent); cleanupErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("close Vector stop event after failed open: %w", cleanupErr))
			}
		}
		err = errors.Join(err, errors.Join(cleanupErrors...))
	}()

	if err := processXLDriver.acquire(api.open); err != nil {
		return nil, err
	}
	driverAcquired = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	permission := access
	application := []byte(vectorApplication)
	interfaceVersion := uintptr(xlInterfaceVersion)
	if config.fd() {
		interfaceVersion = xlInterfaceVersionFD
	}
	result, _, _ := api.openPort.Call(
		uintptr(unsafe.Pointer(&port)),
		uintptr(unsafe.Pointer(&application[0])),
		uintptr(access),
		uintptr(unsafe.Pointer(&permission)),
		xlReceiveQueueSize,
		interfaceVersion,
		xlBusTypeCAN,
	)
	if status := xlStatus(result); status != xlSuccess {
		return nil, api.statusError("open Vector port", status)
	}
	portOpen = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if permission&access != access {
		return nil, errors.New("open Vector port: channel initialization access was not granted")
	}

	if config.fd() {
		fdConfig := newXLCanFDConfig(config)
		result, _, _ = api.setFDConfiguration.Call(
			portArgument(port),
			uintptr(access),
			uintptr(unsafe.Pointer(&fdConfig)),
		)
		if status := xlStatus(result); status != xlSuccess {
			return nil, api.statusError("set Vector CAN FD configuration", status)
		}
	} else {
		result, _, _ = api.setBitrate.Call(portArgument(port), uintptr(access), uintptr(config.Bitrate))
		if status := xlStatus(result); status != xlSuccess {
			return nil, api.statusError("set Vector channel bitrate", status)
		}
	}

	// Do not request TX receipts while accepted transmissions are recorded by
	// Send. They require a separate Capture representation to avoid duplicates.
	result, _, _ = api.setChannelMode.Call(portArgument(port), uintptr(access), 0, 0)
	if status := xlStatus(result); status != xlSuccess {
		return nil, api.statusError("disable Vector transmit receipts", status)
	}

	var receiveEvent windows.Handle
	result, _, _ = api.setNotification.Call(
		portArgument(port),
		uintptr(unsafe.Pointer(&receiveEvent)),
		1,
	)
	if status := xlStatus(result); status != xlSuccess {
		return nil, api.statusError("register Vector receive notification", status)
	}
	if receiveEvent == 0 {
		return nil, errors.New("register Vector receive notification: driver returned a null handle")
	}
	// The XL port owns the notification handle returned by xlSetNotification;
	// xlClosePort releases it. Closing it independently with CloseHandle races
	// the driver and can invalidate its wait object.

	stopEvent, err = windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("create Vector stop event: %w", err)
	}

	result, _, _ = api.activateChannel.Call(portArgument(port), uintptr(access), xlBusTypeCAN, 0)
	if status := xlStatus(result); status != xlSuccess {
		return nil, api.statusError("activate Vector channel", status)
	}
	channelActive = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	result, _, _ = api.requestChipState.Call(portArgument(port), uintptr(access))
	if status := xlStatus(result); status != xlSuccess {
		return nil, api.statusError("request initial Vector chip state", status)
	}
	chipStateRequested := time.Now()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	bus := &Bus{
		id:           config.ID,
		name:         config.Name,
		capture:      capture,
		port:         port,
		access:       access,
		api:          api,
		fd:           config.fd(),
		receiveEvent: receiveEvent,
		stopEvent:    stopEvent,
		lifecycle: driverstate.New(func() {
			_ = windows.SetEvent(stopEvent)
		}),
		nextChipStateRequest: chipStateRequested.Add(chipStateRequestInterval),
		chipStateRefresh:     true,
	}
	go bus.receiveLoop()

	openedBus = bus
	return openedBus, nil
}

type xlAPI struct {
	openDriver         *windows.LazyProc
	closeDriver        *windows.LazyProc
	openPort           *windows.LazyProc
	closePort          *windows.LazyProc
	setNotification    *windows.LazyProc
	setChannelMode     *windows.LazyProc
	setBitrate         *windows.LazyProc
	setFDConfiguration *windows.LazyProc
	activateChannel    *windows.LazyProc
	deactivateChannel  *windows.LazyProc
	requestChipState   *windows.LazyProc
	receive            *windows.LazyProc
	receiveFD          *windows.LazyProc
	transmit           *windows.LazyProc
	transmitFD         *windows.LazyProc
}

func loadXLAPI(fd bool) (*xlAPI, error) {
	dll := windows.NewLazySystemDLL("vxlapi64.dll")
	if err := dll.Load(); err != nil {
		return nil, fmt.Errorf("load vxlapi64.dll from the Windows system directory: %w", err)
	}

	api := &xlAPI{
		openDriver:         dll.NewProc("xlOpenDriver"),
		closeDriver:        dll.NewProc("xlCloseDriver"),
		openPort:           dll.NewProc("xlOpenPort"),
		closePort:          dll.NewProc("xlClosePort"),
		setNotification:    dll.NewProc("xlSetNotification"),
		setChannelMode:     dll.NewProc("xlCanSetChannelMode"),
		setBitrate:         dll.NewProc("xlCanSetChannelBitrate"),
		setFDConfiguration: dll.NewProc("xlCanFdSetConfiguration"),
		activateChannel:    dll.NewProc("xlActivateChannel"),
		deactivateChannel:  dll.NewProc("xlDeactivateChannel"),
		requestChipState:   dll.NewProc("xlCanRequestChipState"),
		receive:            dll.NewProc("xlReceive"),
		receiveFD:          dll.NewProc("xlCanReceive"),
		transmit:           dll.NewProc("xlCanTransmit"),
		transmitFD:         dll.NewProc("xlCanTransmitEx"),
	}
	procedures := []*windows.LazyProc{
		api.openDriver,
		api.closeDriver,
		api.openPort,
		api.closePort,
		api.setNotification,
		api.setChannelMode,
		api.activateChannel,
		api.deactivateChannel,
		api.requestChipState,
	}
	if fd {
		procedures = append(procedures, api.setFDConfiguration, api.receiveFD, api.transmitFD)
	} else {
		procedures = append(procedures, api.setBitrate, api.receive, api.transmit)
	}
	for _, procedure := range procedures {
		if err := procedure.Find(); err != nil {
			return nil, fmt.Errorf("resolve vxlapi64.dll %s: %w", procedure.Name, err)
		}
	}
	return api, nil
}

func (api *xlAPI) statusError(operation string, status xlStatus) error {
	// Keep the native status in the error text until a fourth driver establishes
	// whether a shared typed native-error model is useful.
	return fmt.Errorf("%s: Vector status %d", operation, status)
}

func (api *xlAPI) open() error {
	result, _, _ := api.openDriver.Call()
	if status := xlStatus(result); status != xlSuccess {
		return api.statusError("open Vector driver", status)
	}
	return nil
}

func (api *xlAPI) close() error {
	result, _, _ := api.closeDriver.Call()
	if status := xlStatus(result); status != xlSuccess {
		return api.statusError("close Vector driver", status)
	}
	return nil
}

func portArgument(port xlPortHandle) uintptr {
	return uintptr(uint32(port))
}

// Bus is one open Vector CAN channel.
type Bus struct {
	id      gocan.BusID
	name    string
	capture *gocan.Capture
	port    xlPortHandle
	access  xlAccess
	api     *xlAPI
	fd      bool

	receiveEvent windows.Handle
	stopEvent    windows.Handle

	sendMu    sync.Mutex
	lifecycle *driverstate.Lifecycle

	cleanupErr error

	// The receive goroutine is the only writer after Open publishes the bus.
	nextChipStateRequest time.Time
	chipStateRefresh     bool
	lastChipState        gocan.Event
	hasChipState         bool
}

var _ gocan.Bus = (*Bus)(nil)

// ID returns the one-based trace channel assigned to this bus.
func (bus *Bus) ID() gocan.BusID { return bus.id }

// Name returns the human-readable name of this bus.
func (bus *Bus) Name() string { return bus.name }

// Send hands frame to the XL Driver Library and records an accepted transmission.
func (bus *Bus) Send(ctx context.Context, frame gocan.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.Flags.Has(gocan.FrameFD) && !bus.fd {
		return errors.New("Vector classic bus cannot send a CAN FD frame")
	}
	if !frame.Flags.Has(gocan.FrameFD) && frame.DLC > 8 {
		return fmt.Errorf("Vector does not yet support classic DLC %d", frame.DLC)
	}
	if frame.Flags.Has(gocan.FrameErrorStateIndicator) {
		return errors.New("Vector cannot request the CAN FD error state indicator on transmit")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bus.lifecycle.StopSignal():
		return bus.operationError()
	default:
	}

	bus.sendMu.Lock()
	defer bus.sendMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bus.lifecycle.StopSignal():
		return bus.operationError()
	default:
	}

	if err := bus.transmitFrame(frame); err != nil {
		if errors.Is(err, gocan.ErrHardwareDisconnected) {
			bus.stopWithError(err)
		}
		return err
	}

	if err := bus.capture.Append(gocan.FrameEvent{
		Bus:       bus.id,
		Timestamp: time.Now(),
		Direction: gocan.DirectionTransmit,
		Frame:     frame,
	}); err != nil {
		bus.stopWithError(err)
		return err
	}
	return nil
}

func (bus *Bus) transmitFrame(frame gocan.Frame) error {
	if bus.fd {
		event := encodeFDTransmitEvent(frame)
		messageCount := uint32(0)
		result, _, _ := bus.api.transmitFD.Call(
			portArgument(bus.port),
			uintptr(bus.access),
			1,
			uintptr(unsafe.Pointer(&messageCount)),
			uintptr(unsafe.Pointer(&event)),
		)
		if status := xlStatus(result); status != xlSuccess {
			if status == xlQueueFull {
				return fmt.Errorf("%w: Vector status %d", gocan.ErrTransmitQueueFull, status)
			}
			return bus.runtimeStatusError("send Vector CAN FD frame", status)
		}
		if messageCount != 1 {
			return fmt.Errorf("send Vector CAN FD frame: accepted %d of 1 frames", messageCount)
		}
		return nil
	}

	event := encodeEvent(frame)
	messageCount := uint32(1)
	result, _, _ := bus.api.transmit.Call(
		portArgument(bus.port),
		uintptr(bus.access),
		uintptr(unsafe.Pointer(&messageCount)),
		uintptr(unsafe.Pointer(&event)),
	)
	if status := xlStatus(result); status != xlSuccess {
		if status == xlQueueFull {
			return fmt.Errorf("%w: Vector status %d", gocan.ErrTransmitQueueFull, status)
		}
		return bus.runtimeStatusError("send Vector frame", status)
	}
	if messageCount != 1 {
		return fmt.Errorf("send Vector frame: accepted %d of 1 frames", messageCount)
	}
	return nil
}

// Done is closed when this bus stops.
func (bus *Bus) Done() <-chan struct{} { return bus.lifecycle.Done() }

// Err returns the background failure that stopped this bus, if any.
func (bus *Bus) Err() error {
	return bus.lifecycle.Err()
}

// Close stops acquisition and releases the Vector channel and port.
func (bus *Bus) Close() error {
	bus.stopWithError(nil)
	<-bus.lifecycle.Done()
	return bus.cleanupErr
}

func (bus *Bus) receiveLoop() {
	defer bus.finish()

	events := []windows.Handle{bus.stopEvent, bus.receiveEvent}
	for {
		now := time.Now()
		if !now.Before(bus.nextChipStateRequest) {
			if err := bus.requestChipState(now); err != nil {
				bus.stopWithError(err)
				return
			}
			continue
		}
		remaining := time.Until(bus.nextChipStateRequest)
		timeout := uint32(max(1, (remaining+time.Millisecond-1)/time.Millisecond))

		event, err := windows.WaitForMultipleObjects(events, false, timeout)
		if err != nil {
			bus.stopWithError(fmt.Errorf("wait for Vector receive event: %w", err))
			return
		}
		switch event {
		case windows.WAIT_OBJECT_0:
			return
		case windows.WAIT_OBJECT_0 + 1:
			stopped, err := bus.drainReceiveQueue()
			if err != nil {
				bus.stopWithError(err)
				return
			}
			if stopped {
				return
			}
		case uint32(windows.WAIT_TIMEOUT):
			continue
		default:
			bus.stopWithError(fmt.Errorf("wait for Vector receive event: unexpected result %#x", event))
			return
		}
	}
}

func (bus *Bus) drainReceiveQueue() (bool, error) {
	if bus.fd {
		return bus.drainFDReceiveQueue()
	}
	return bus.drainClassicReceiveQueue()
}

func (bus *Bus) drainClassicReceiveQueue() (bool, error) {
	for {
		select {
		case <-bus.lifecycle.StopSignal():
			return true, nil
		default:
		}

		var event xlEvent
		messageCount := uint32(1)
		result, _, _ := bus.api.receive.Call(
			portArgument(bus.port),
			uintptr(unsafe.Pointer(&messageCount)),
			uintptr(unsafe.Pointer(&event)),
		)
		timestamp := time.Now()
		status := xlStatus(result)
		switch status {
		case xlQueueEmpty:
			return false, nil
		case xlQueueOverrun:
			observation, err := newOverrunObservation(
				bus.id,
				timestamp,
				fmt.Sprintf("Vector receive status %d", status),
			)
			if err != nil {
				return false, err
			}
			return bus.applyObservation(observation, timestamp)
		case xlSuccess:
		default:
			return false, bus.runtimeStatusError("receive Vector event", status)
		}
		if messageCount != 1 {
			return false, fmt.Errorf("receive Vector event: driver returned %d events", messageCount)
		}

		observation, err := decodeClassicReceiveEvent(&event, bus.id, timestamp)
		if err != nil {
			return false, err
		}
		stopped, err := bus.applyObservation(observation, timestamp)
		if err != nil || stopped {
			return stopped, err
		}
	}
}

func (bus *Bus) drainFDReceiveQueue() (bool, error) {
	for {
		select {
		case <-bus.lifecycle.StopSignal():
			return true, nil
		default:
		}

		var event xlCANRXEvent
		result, _, _ := bus.api.receiveFD.Call(
			portArgument(bus.port),
			uintptr(unsafe.Pointer(&event)),
		)
		timestamp := time.Now()
		status := xlStatus(result)
		switch status {
		case xlQueueEmpty:
			return false, nil
		case xlQueueOverrun:
			observation, err := newOverrunObservation(
				bus.id,
				timestamp,
				fmt.Sprintf("Vector CAN FD receive status %d", status),
			)
			if err != nil {
				return false, err
			}
			return bus.applyObservation(observation, timestamp)
		case xlSuccess:
		default:
			return false, bus.runtimeStatusError("receive Vector CAN FD event", status)
		}

		observation, err := decodeFDReceiveEvent(&event, bus.id, timestamp)
		if err != nil {
			return false, err
		}
		stopped, err := bus.applyObservation(observation, timestamp)
		if err != nil || stopped {
			return stopped, err
		}
	}
}

func (bus *Bus) applyObservation(observation receiveObservation, timestamp time.Time) (bool, error) {
	if observation.terminal != nil {
		bus.stopWithEvents(observation.terminal, observation.events[:observation.eventCount])
		return true, nil
	}
	for index := range observation.eventCount {
		event := observation.events[index]
		switch event.Kind {
		case gocan.EventErrorFrame:
			bus.setChipStateRefresh(true, timestamp)
		case gocan.EventControllerState:
			bus.setChipStateRefresh(event.ControllerState != gocan.ControllerActive, timestamp)
			if bus.hasChipState && sameChipState(bus.lastChipState, event) {
				continue
			}
			bus.lastChipState = event
			bus.hasChipState = true
		}
		if err := bus.capture.AppendEvent(event); err != nil {
			return false, err
		}
	}
	if observation.requestChipState {
		if err := bus.requestChipState(timestamp); err != nil {
			return false, err
		}
	}
	if observation.hasFrame {
		if err := bus.capture.Append(gocan.FrameEvent{
			Bus:       bus.id,
			Timestamp: timestamp,
			Direction: gocan.DirectionReceive,
			Frame:     observation.frame,
		}); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (bus *Bus) requestChipState(now time.Time) error {
	if now.Before(bus.nextChipStateRequest) {
		return nil
	}
	result, _, _ := bus.api.requestChipState.Call(portArgument(bus.port), uintptr(bus.access))
	if status := xlStatus(result); status != xlSuccess {
		return bus.runtimeStatusError("request Vector chip state", status)
	}
	interval := chipStateHealthInterval
	if bus.chipStateRefresh {
		interval = chipStateRequestInterval
	}
	bus.nextChipStateRequest = now.Add(interval)
	return nil
}

func (bus *Bus) setChipStateRefresh(refresh bool, now time.Time) {
	if bus.chipStateRefresh == refresh {
		return
	}
	bus.chipStateRefresh = refresh
	if refresh {
		refreshAt := now.Add(chipStateRequestInterval)
		if refreshAt.Before(bus.nextChipStateRequest) {
			bus.nextChipStateRequest = refreshAt
		}
		return
	}
	bus.nextChipStateRequest = now.Add(chipStateHealthInterval)
}

func (bus *Bus) runtimeStatusError(operation string, status xlStatus) error {
	if status == xlInvalidHandle {
		return fmt.Errorf(
			"%w: %s: Vector status %d (XL_ERR_INVALID_HANDLE)",
			gocan.ErrHardwareDisconnected,
			operation,
			status,
		)
	}
	return bus.api.statusError(operation, status)
}

func sameChipState(first, second gocan.Event) bool {
	return first.ControllerState == second.ControllerState &&
		first.TXErrorCount == second.TXErrorCount &&
		first.RXErrorCount == second.RXErrorCount &&
		first.ErrorCountsKnown == second.ErrorCountsKnown
}

func (bus *Bus) stopWithError(err error) {
	bus.lifecycle.Stop(err)
}

func (bus *Bus) stopWithEvents(terminal error, events []gocan.Event) {
	bus.sendMu.Lock()
	defer bus.sendMu.Unlock()

	for _, event := range events {
		if err := bus.capture.AppendEvent(event); err != nil {
			bus.stopWithError(err)
			return
		}
	}
	bus.stopWithError(terminal)
}

func (bus *Bus) finish() {
	bus.sendMu.Lock()
	bus.cleanupErr = bus.cleanup()
	bus.sendMu.Unlock()
	bus.lifecycle.MarkDone()
}

func (bus *Bus) cleanup() error {
	var cleanupErrors []error
	result, _, _ := bus.api.deactivateChannel.Call(portArgument(bus.port), uintptr(bus.access))
	if status := xlStatus(result); status != xlSuccess {
		cleanupErrors = append(cleanupErrors, bus.api.statusError("deactivate Vector channel", status))
	}
	result, _, _ = bus.api.closePort.Call(portArgument(bus.port))
	if status := xlStatus(result); status != xlSuccess {
		cleanupErrors = append(cleanupErrors, bus.api.statusError("close Vector port", status))
	}
	if err := processXLDriver.release(bus.api.close); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := windows.CloseHandle(bus.stopEvent); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("close Vector stop event: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

func (bus *Bus) operationError() error {
	return bus.lifecycle.OperationError()
}
