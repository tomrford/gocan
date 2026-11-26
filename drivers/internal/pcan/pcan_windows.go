//go:build windows

package pcan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/internal/drivercfg"
	"github.com/tomrford/gocan/internal/devicechange"
	"github.com/tomrford/gocan/internal/driverstate"
	"golang.org/x/sys/windows"
)

// channelSettleDelay covers a brief post-initialization interval during which
// PCAN-Basic can silently lose the first received frame. It exposes no
// readiness signal, so retain margin over the observed roughly 3 ms window.
const channelSettleDelay = 25 * time.Millisecond

// maxDrainBatch bounds how many queued native reads run before receiveLoop
// services due controller-status polls.
const maxDrainBatch = 64

// A PEAK USB removal is one shared PCAN process failure domain: every open
// PCAN bus stops, because a classic PCAN-USB handle cannot be reliably mapped
// to one Windows device instance.
var processPCANDevices = devicechange.NewMonitor("PEAK USB", `USB\VID_0C72&`)

type ChannelCondition uint32

const (
	ChannelConditionUnavailable ChannelCondition = iota
	ChannelConditionAvailable
	ChannelConditionOccupied
	// ChannelConditionPCANView means that the channel is in use by PCAN-View
	// but remains available to initialize.
	ChannelConditionPCANView
)

type ChannelInfo struct {
	Channel          Channel
	Name             string
	DeviceID         uint32
	ControllerNumber uint8
	SupportsFD       bool
	Condition        ChannelCondition
}

// Discover reports the attached PCAN channels without initializing them.
// The result includes occupied channels in the order reported by PCAN-Basic.
func Discover() ([]ChannelInfo, error) {
	api, err := loadPCANAPI()
	if err != nil {
		return nil, err
	}
	return queryAttachedPCANChannels(api)
}

// Open initializes a PCAN-Basic channel and starts capturing received frames.
//
// Open waits through the measured settle window, so a caller may Send
// immediately; see channelSettleDelay. Context controls opening only;
// canceling it after Open returns does not stop the bus.
func Open(ctx context.Context, capture *gocan.Capture, config Config) (openedBus *Bus, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd := config.FDBitrate != ""
	var bitrateCode uint16
	if !fd {
		code, ok := classicalBitrates[config.Bitrate]
		if !ok {
			return nil, fmt.Errorf("%w: PCAN classical bitrate %d is not a PCAN-Basic predefined rate", drivercfg.ErrInvalidConfig, config.Bitrate)
		}
		bitrateCode = code
	}
	api, err := loadPCANAPI()
	if err != nil {
		return nil, err
	}

	// The hold must pin the device generation before CAN_Initialize so a
	// removal during initialization cannot slip between inventory and Bind.
	deviceHold, err := processPCANDevices.Hold()
	if err != nil {
		return nil, err
	}
	defer func() {
		if openedBus == nil {
			err = errors.Join(err, deviceHold.Release())
		}
	}()

	var (
		result uintptr
		status pcanStatus
	)
	if fd {
		timing := append([]byte(config.FDBitrate), 0)
		result, _, _ = api.initializeFD.Call(
			uintptr(config.Channel),
			uintptr(unsafe.Pointer(&timing[0])),
		)
		status = pcanStatus(result)
	} else {
		result, _, _ = api.initialize.Call(
			uintptr(config.Channel),
			uintptr(bitrateCode),
			0,
			0,
			0,
		)
		status = pcanStatus(result)
	}
	if status != pcanStatusOK {
		initializationError := api.statusError("initialize PCAN channel", status)
		if status == pcanStatusCaution {
			result, _, _ := api.uninitialize.Call(uintptr(config.Channel))
			if cleanupStatus := pcanStatus(result); cleanupStatus != pcanStatusOK {
				initializationError = errors.Join(
					initializationError,
					api.statusError("release PCAN channel after cautious initialization", cleanupStatus),
				)
			}
		}
		return nil, initializationError
	}

	initialized := time.Now()

	uninitialize := func() {
		_, _, _ = api.uninitialize.Call(uintptr(config.Channel))
	}

	parameterOn := uint32(1)
	for _, parameter := range []struct {
		value     uintptr
		operation string
	}{
		{pcanAllowStatusFrames, "enable PCAN status frames"},
		{pcanAllowErrorFrames, "enable PCAN error frames"},
		{pcanBusOffAutoReset, "enable PCAN bus-off auto-reset"},
	} {
		result, _, _ = api.setValue.Call(
			uintptr(config.Channel),
			parameter.value,
			uintptr(unsafe.Pointer(&parameterOn)),
			unsafe.Sizeof(parameterOn),
		)
		if status = pcanStatus(result); status != pcanStatusOK {
			uninitialize()
			return nil, api.statusError(parameter.operation, status)
		}
	}
	attached, err := queryAttachedPCANChannels(api)
	if err != nil {
		uninitialize()
		return nil, err
	}
	channelIndex := slices.IndexFunc(attached, func(info ChannelInfo) bool { return info.Channel == config.Channel })
	if channelIndex < 0 {
		uninitialize()
		return nil, missingPCANChannelError(config.Channel)
	}
	classicHardware := !attached[channelIndex].SupportsFD

	receiveEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		uninitialize()
		return nil, fmt.Errorf("create PCAN receive event: %w", err)
	}
	stopEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(receiveEvent)
		uninitialize()
		return nil, fmt.Errorf("create PCAN stop event: %w", err)
	}

	if uint64(receiveEvent) > uint64(^uint32(0)) {
		_ = windows.CloseHandle(stopEvent)
		_ = windows.CloseHandle(receiveEvent)
		uninitialize()
		return nil, fmt.Errorf("register PCAN receive event: Windows handle %#x does not fit the required uint32 buffer", uintptr(receiveEvent))
	}
	eventValue := uint32(receiveEvent)
	result, _, _ = api.setValue.Call(
		uintptr(config.Channel),
		pcanReceiveEvent,
		uintptr(unsafe.Pointer(&eventValue)),
		unsafe.Sizeof(eventValue),
	)
	if status = pcanStatus(result); status != pcanStatusOK {
		_ = windows.CloseHandle(stopEvent)
		_ = windows.CloseHandle(receiveEvent)
		uninitialize()
		return nil, api.statusError("register PCAN receive event", status)
	}

	bus := &Bus{
		id:           config.ID,
		name:         config.Name,
		capture:      capture,
		channel:      config.Channel,
		fd:           fd,
		api:          api,
		receiveEvent: receiveEvent,
		stopEvent:    stopEvent,
		lifecycle: driverstate.New(func() {
			_ = windows.SetEvent(stopEvent)
		}),
		state: pcanState{classic: classicHardware},
	}
	subscription, err := deviceHold.Bind(bus.stopWithError)
	if err != nil {
		bus.cleanupErr = bus.cleanup()
		return nil, errors.Join(err, bus.cleanupErr)
	}
	bus.deviceSubscription = subscription
	go bus.receiveLoop()

	settle := time.NewTimer(time.Until(initialized.Add(channelSettleDelay)))
	defer settle.Stop()
	select {
	case <-settle.C:
		return bus, nil
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), bus.Close())
	case <-bus.lifecycle.Done():
		return nil, errors.Join(bus.operationError(), bus.cleanupErr)
	}
}

type pcanAPI struct {
	initialize   *windows.LazyProc
	initializeFD *windows.LazyProc
	uninitialize *windows.LazyProc
	read         *windows.LazyProc
	readFD       *windows.LazyProc
	write        *windows.LazyProc
	writeFD      *windows.LazyProc
	getStatus    *windows.LazyProc
	getValue     *windows.LazyProc
	setValue     *windows.LazyProc
	getErrorText *windows.LazyProc
}

func loadPCANAPI() (*pcanAPI, error) {
	dll := windows.NewLazySystemDLL("PCANBasic.dll")
	if err := dll.Load(); err != nil {
		if errors.Is(err, windows.ERROR_MOD_NOT_FOUND) {
			systemDirectory, directoryErr := windows.GetSystemDirectory()
			if directoryErr == nil {
				_, statErr := os.Stat(filepath.Join(systemDirectory, dll.Name))
				if os.IsNotExist(statErr) {
					return nil, fmt.Errorf("%w: load PCANBasic.dll: %v", gocan.ErrDriverUnavailable, err)
				}
			}
		}
		return nil, fmt.Errorf("load PCANBasic.dll from the Windows system directory: %w", err)
	}

	api := &pcanAPI{
		initialize:   dll.NewProc("CAN_Initialize"),
		initializeFD: dll.NewProc("CAN_InitializeFD"),
		uninitialize: dll.NewProc("CAN_Uninitialize"),
		read:         dll.NewProc("CAN_Read"),
		readFD:       dll.NewProc("CAN_ReadFD"),
		write:        dll.NewProc("CAN_Write"),
		writeFD:      dll.NewProc("CAN_WriteFD"),
		getStatus:    dll.NewProc("CAN_GetStatus"),
		getValue:     dll.NewProc("CAN_GetValue"),
		setValue:     dll.NewProc("CAN_SetValue"),
		getErrorText: dll.NewProc("CAN_GetErrorText"),
	}
	for _, procedure := range []*windows.LazyProc{
		api.initialize,
		api.initializeFD,
		api.uninitialize,
		api.read,
		api.readFD,
		api.write,
		api.writeFD,
		api.getStatus,
		api.getValue,
		api.setValue,
		api.getErrorText,
	} {
		if err := procedure.Find(); err != nil {
			return nil, fmt.Errorf("resolve PCANBasic.dll %s: %w", procedure.Name, err)
		}
	}
	return api, nil
}

func (api *pcanAPI) statusError(operation string, status pcanStatus) error {
	if text, ok := api.errorText(status); ok {
		return fmt.Errorf("%s: PCAN status %#08x: %s", operation, uint32(status), text)
	}
	// CAN_GetErrorText can reject combined bitmasks such as BUSOFF|BUSLIGHT;
	// resolve each set bit individually when the combined lookup fails.
	var parts []string
	for remaining := uint32(status); remaining != 0; {
		bit := remaining & -remaining
		remaining ^= bit
		if text, ok := api.errorText(pcanStatus(bit)); ok {
			parts = append(parts, text)
		}
	}
	if len(parts) > 0 {
		return fmt.Errorf("%s: PCAN status %#08x: %s", operation, uint32(status), strings.Join(parts, "; "))
	}
	return fmt.Errorf("%s: PCAN status %#08x", operation, uint32(status))
}

func (api *pcanAPI) errorText(status pcanStatus) (string, bool) {
	var textBuffer [256]byte
	result, _, _ := api.getErrorText.Call(
		uintptr(status),
		0,
		uintptr(unsafe.Pointer(&textBuffer[0])),
	)
	if pcanStatus(result) != pcanStatusOK {
		return "", false
	}
	length := 0
	for length < len(textBuffer) && textBuffer[length] != 0 {
		length++
	}
	if length == 0 {
		return "", false
	}
	return string(textBuffer[:length]), true
}

func queryAttachedPCANChannels(api *pcanAPI) ([]ChannelInfo, error) {
	var count uint32
	result, _, _ := api.getValue.Call(
		0,
		pcanAttachedCount,
		uintptr(unsafe.Pointer(&count)),
		unsafe.Sizeof(count),
	)
	if status := pcanStatus(result); status != pcanStatusOK {
		return nil, api.statusError("query attached PCAN channel count", status)
	}
	if count > 256 {
		return nil, fmt.Errorf("query attached PCAN channels: implausible channel count %d", count)
	}
	if count == 0 {
		return nil, nil
	}

	information := make([]pcanChannelInformation, count)
	result, _, _ = api.getValue.Call(
		0,
		pcanAttachedChannels,
		uintptr(unsafe.Pointer(&information[0])),
		uintptr(len(information))*unsafe.Sizeof(information[0]),
	)
	if status := pcanStatus(result); status != pcanStatusOK {
		return nil, api.statusError("query attached PCAN channels", status)
	}
	channels := make([]ChannelInfo, len(information))
	for index := range information {
		channels[index] = mapPCANChannelInformation(information[index])
	}
	return channels, nil
}

func mapPCANChannelInformation(information pcanChannelInformation) ChannelInfo {
	name := information.deviceName[:]
	if end := bytes.IndexByte(name, 0); end >= 0 {
		name = name[:end]
	}
	return ChannelInfo{
		Channel:          information.channelHandle,
		Name:             string(name),
		DeviceID:         information.deviceID,
		ControllerNumber: information.controllerNumber,
		SupportsFD:       information.deviceFeatures&pcanFeatureFD != 0,
		Condition:        ChannelCondition(information.channelCondition),
	}
}

func missingPCANChannelError(channel Channel) error {
	return fmt.Errorf(
		"%w: PCAN channel %#x is absent from the attached-channel inventory",
		gocan.ErrHardwareDisconnected,
		uint16(channel),
	)
}

type Bus struct {
	id      gocan.BusID
	name    string
	capture *gocan.Capture
	channel Channel
	fd      bool
	api     *pcanAPI

	receiveEvent windows.Handle
	stopEvent    windows.Handle

	ioMu      sync.Mutex
	lifecycle *driverstate.Lifecycle

	// Guarded by ioMu.
	state pcanState

	deviceSubscription *devicechange.Subscription
	cleanupErr         error
}

var _ gocan.Bus = (*Bus)(nil)

func (bus *Bus) ID() gocan.BusID {
	return bus.id
}

func (bus *Bus) Name() string {
	return bus.name
}

func (bus *Bus) Capture() *gocan.Capture {
	return bus.capture
}

func (bus *Bus) Send(ctx context.Context, frame gocan.Frame) error {
	if err := validateSendFrame(frame, bus.fd); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bus.lifecycle.StopSignal():
		return bus.operationError()
	default:
	}

	bus.ioMu.Lock()
	defer bus.ioMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bus.lifecycle.StopSignal():
		return bus.operationError()
	default:
	}

	// Avoid calling CAN_Write while bus-off: writes every <= 20 ms with an
	// acknowledging peer keep PCAN-USB stuck in bus-off instead of letting
	// auto-reset complete. Only live CAN_GetStatus polling clears bus-off.
	if bus.state.busOff() {
		return gocan.ErrBusOff
	}

	var status pcanStatus
	if bus.fd {
		message := encodeFDMessage(frame)
		result, _, _ := bus.api.writeFD.Call(
			uintptr(bus.channel),
			uintptr(unsafe.Pointer(&message)),
		)
		status = pcanStatus(result)
	} else {
		message := encodeClassicMessage(frame)
		result, _, _ := bus.api.write.Call(
			uintptr(bus.channel),
			uintptr(unsafe.Pointer(&message)),
		)
		status = pcanStatus(result)
	}
	if status != pcanStatusOK {
		if status&pcanStatusBusOff != 0 {
			if event, ok := bus.state.observeSendBusOff(time.Now(), bus.id); ok {
				if err := bus.capture.RecordEvent(event); err != nil {
					bus.stopWithError(err)
					return err
				}
			}
			_ = windows.SetEvent(bus.receiveEvent)
			return fmt.Errorf("%w: PCAN status %#08x", gocan.ErrBusOff, uint32(status))
		}
		if status&pcanStatusTXFullMask != 0 {
			return fmt.Errorf("%w: PCAN status %#08x", gocan.ErrTransmitQueueFull, uint32(status))
		}
		err := bus.runtimeStatusError("send PCAN frame", status)
		if errors.Is(err, gocan.ErrHardwareDisconnected) {
			bus.stopWithError(err)
		}
		return err
	}

	bus.state.observeSend()

	if err := bus.capture.RecordFrame(gocan.FrameEvent{
		Bus:       bus.id,
		Direction: gocan.DirectionTransmit,
		Frame:     frame,
	}); err != nil {
		bus.stopWithError(err)
		return err
	}
	return nil
}

func (bus *Bus) Context() context.Context {
	return bus.lifecycle.Context()
}

func (bus *Bus) Close() error {
	bus.stopWithError(nil)
	<-bus.lifecycle.Done()
	return bus.cleanupErr
}

func (bus *Bus) receiveLoop() {
	defer bus.finish()

	events := []windows.Handle{bus.stopEvent, bus.receiveEvent}
	pendingDrain := false
	for {
		nextDeadline, err := bus.serviceState(!pendingDrain)
		if err != nil {
			bus.stopWithError(err)
			return
		}
		if pendingDrain {
			pendingDrain, err = bus.drainReceiveQueue()
			if err != nil {
				bus.stopWithError(err)
				return
			}
			continue
		}
		timeout := uint32(windows.INFINITE)
		if !nextDeadline.IsZero() {
			remaining := time.Until(nextDeadline)
			timeout = uint32(max(1, (remaining+time.Millisecond-1)/time.Millisecond))
		}
		event, err := windows.WaitForMultipleObjects(events, false, timeout)
		if err != nil {
			bus.stopWithError(fmt.Errorf("wait for PCAN receive event: %w", err))
			return
		}
		switch event {
		case windows.WAIT_OBJECT_0:
			return
		case windows.WAIT_OBJECT_0 + 1, uint32(windows.WAIT_TIMEOUT):
			pendingDrain, err = bus.drainReceiveQueue()
			if err != nil {
				bus.stopWithError(err)
				return
			}
		default:
			bus.stopWithError(fmt.Errorf("wait for PCAN receive event: unexpected result %#x", event))
			return
		}
	}
}

func (bus *Bus) runtimeStatusError(operation string, status pcanStatus) error {
	if status&pcanStatusHandleMask == pcanStatusInvalidHW {
		return fmt.Errorf(
			"%w: %s: PCAN status %#08x (PCAN_ERROR_ILLHW)",
			gocan.ErrHardwareDisconnected,
			operation,
			uint32(status),
		)
	}
	return bus.api.statusError(operation, status)
}

func (bus *Bus) drainReceiveQueue() (bool, error) {
	for range maxDrainBatch {
		more, err := bus.receiveOne()
		if err != nil {
			return false, err
		}
		if !more {
			return false, nil
		}
	}
	return true, nil
}

func (bus *Bus) serviceState(queueDrained bool) (time.Time, error) {
	bus.ioMu.Lock()
	defer bus.ioMu.Unlock()

	select {
	case <-bus.lifecycle.StopSignal():
		return time.Time{}, nil
	default:
	}
	now := time.Now()
	if !bus.state.nextPoll.IsZero() && !now.Before(bus.state.nextPoll) {
		result, _, _ := bus.api.getStatus.Call(uintptr(bus.channel))
		status := pcanStatus(result)
		if status&^pcanStatusKnownMask != 0 {
			return time.Time{}, bus.runtimeStatusError("read PCAN status", status)
		}
		events, count, err := bus.state.pollStatus(now, status, bus.id, queueDrained)
		if err != nil {
			return time.Time{}, err
		}
		for _, event := range events[:count] {
			if err := bus.capture.RecordEvent(event); err != nil {
				return time.Time{}, err
			}
		}
	}
	return bus.state.nextDeadline(), nil
}

// receiveOne serialises a single native read and capture append with Send.
// The caller waits for receive readiness without holding ioMu.
func (bus *Bus) receiveOne() (bool, error) {
	bus.ioMu.Lock()
	defer bus.ioMu.Unlock()

	select {
	case <-bus.lifecycle.StopSignal():
		return false, nil
	default:
	}

	var (
		id          uint32
		messageType uint8
		dlc         uint8
		data        []byte
		status      pcanStatus
	)
	if bus.fd {
		var message pcanMsgFD
		result, _, _ := bus.api.readFD.Call(
			uintptr(bus.channel),
			uintptr(unsafe.Pointer(&message)),
			0,
		)
		status = pcanStatus(result)
		id, messageType, dlc, data = message.id, message.messageType, message.dlc, message.data[:]
	} else {
		var message pcanMsg
		result, _, _ := bus.api.read.Call(
			uintptr(bus.channel),
			uintptr(unsafe.Pointer(&message)),
			0,
		)
		status = pcanStatus(result)
		id, messageType, dlc, data = message.id, message.messageType, message.length, message.data[:]
	}

	if status&pcanStatusHandleMask == pcanStatusInvalidHW {
		return false, bus.runtimeStatusError("receive PCAN frame", status)
	}
	if status&pcanStatusQueueEmpty != 0 {
		return false, bus.applyQueueEmptyStatusLocked(status)
	}
	// PCAN-Basic reports invalid wire data before delivering the associated
	// enabled error frame. The invalid data itself has already been discarded.
	if status == pcanStatusInvalidData {
		return true, nil
	}

	var (
		observation pcanReceiveObservation
		err         error
	)
	if status&pcanStatusOverrunMask != 0 {
		observation, err = decodePCANStatus(status, bus.id)
	} else {
		observation, err = decodePCANReceive(id, messageType, dlc, data, bus.fd, status, bus.id)
	}
	if err != nil {
		return false, err
	}
	if err := bus.applyObservationLocked(observation); err != nil {
		return false, err
	}
	return true, nil
}

func (bus *Bus) applyQueueEmptyStatusLocked(status pcanStatus) error {
	rest := status &^ pcanStatusQueueEmpty
	if rest == pcanStatusOK {
		return nil
	}
	observation, err := decodePCANStatus(rest, bus.id)
	if err != nil {
		return err
	}
	// Combined with PCAN_ERROR_QRCVEMPTY, so this is not a bare 0x02 return.
	observation.bareHWOverrun = false
	return bus.applyObservationLocked(observation)
}

func (bus *Bus) applyObservationLocked(observation pcanReceiveObservation) error {
	events, count := bus.state.observe(time.Now(), observation)
	for _, event := range events[:count] {
		if err := bus.capture.RecordEvent(event); err != nil {
			return err
		}
	}
	if observation.hasFrame {
		return bus.capture.RecordFrame(gocan.FrameEvent{
			Bus:       bus.id,
			Direction: gocan.DirectionReceive,
			Frame:     observation.frame,
		})
	}
	return nil
}

func (bus *Bus) stopWithError(err error) {
	bus.lifecycle.Stop(err)
}

func (bus *Bus) finish() {
	// Wait for any in-flight Stop wake callback to finish using stopEvent
	// before cleanup closes the handle.
	bus.lifecycle.Stop(nil)
	detachErr := bus.deviceSubscription.Cancel()
	bus.ioMu.Lock()
	bus.cleanupErr = errors.Join(detachErr, bus.cleanup())
	bus.ioMu.Unlock()
	bus.lifecycle.MarkDone()
}

func (bus *Bus) cleanup() error {
	var cleanupErrors []error

	eventValue := uint32(0)
	result, _, _ := bus.api.setValue.Call(
		uintptr(bus.channel),
		pcanReceiveEvent,
		uintptr(unsafe.Pointer(&eventValue)),
		unsafe.Sizeof(eventValue),
	)
	if status := pcanStatus(result); status != pcanStatusOK {
		cleanupErrors = append(cleanupErrors, bus.api.statusError("disable PCAN receive event", status))
	}
	result, _, _ = bus.api.uninitialize.Call(uintptr(bus.channel))
	if status := pcanStatus(result); status != pcanStatusOK {
		cleanupErrors = append(cleanupErrors, bus.api.statusError("uninitialize PCAN channel", status))
	}
	if err := windows.CloseHandle(bus.receiveEvent); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("close PCAN receive event: %w", err))
	}
	if err := windows.CloseHandle(bus.stopEvent); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("close PCAN stop event: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

func (bus *Bus) operationError() error {
	return bus.lifecycle.OperationError()
}
