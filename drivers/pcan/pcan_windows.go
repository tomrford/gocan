//go:build windows

package pcan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/internal/devicechange"
	"github.com/tomrford/gocan/internal/driverstate"
	"golang.org/x/sys/windows"
)

// channelSettleDelay covers a period after CAN_Initialize during which the
// first received frame can be lost rather than delayed.
//
// Hardware qualification showed a short loss window after initialization.
// PCAN_RECEIVE_EVENT increased losses inside that window, and polling CAN_Read
// did not avoid them. PCAN-Basic exposes no usable readiness signal:
// CAN_GetStatus reports OK immediately and initialization queues no activation
// status. Use a generous margin because a short delay fails silently.
//
// TODO: Revalidate this budget on other adapters, hosts, hubs, and driver
// versions with the conformance suite's "first frame after open" case.
const channelSettleDelay = 25 * time.Millisecond

// A PEAK USB removal is one shared PCAN process failure domain: every open
// PCAN bus stops, because a classic PCAN-USB handle cannot be reliably mapped
// to one Windows device instance.
var processPCANDevices = devicechange.NewMonitor(`USB\VID_0C72&`, "PEAK USB")

// ChannelCondition reports the native availability state of a PCAN channel.
type ChannelCondition uint32

const (
	// ChannelConditionUnavailable means that the channel is not available.
	ChannelConditionUnavailable ChannelCondition = iota
	// ChannelConditionAvailable means that the channel is available for use.
	ChannelConditionAvailable
	// ChannelConditionOccupied means that the channel is already in use.
	ChannelConditionOccupied
	// ChannelConditionPCANView means that the channel is in use by PCAN-View
	// but remains available to initialize.
	ChannelConditionPCANView
)

// ChannelInfo describes one channel reported by PCAN-Basic.
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
	if err := validateConfig(capture, config); err != nil {
		return nil, err
	}
	if strings.IndexByte(config.FDBitrate, 0) >= 0 {
		return nil, errors.New("PCAN FD bitrate contains a NUL byte")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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

	fd := config.FDBitrate != ""
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
			uintptr(config.Bitrate),
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

	// Count subsequent setup toward the settle window.
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
	if !slices.ContainsFunc(attached, func(info ChannelInfo) bool { return info.Channel == config.Channel }) {
		uninitialize()
		return nil, missingPCANChannelError(config.Channel)
	}

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
	}
	subscription, err := deviceHold.Bind(bus.stopWithError)
	if err != nil {
		bus.cleanupErr = bus.cleanup()
		return nil, errors.Join(err, bus.cleanupErr)
	}
	bus.deviceSubscription = subscription
	go bus.receiveLoop()

	// Setup performed since CAN_Initialize counts toward the settle.
	settle := time.NewTimer(time.Until(initialized.Add(channelSettleDelay)))
	defer settle.Stop()
	select {
	case <-settle.C:
		return bus, nil
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), bus.Close())
	case <-bus.Done():
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
	getValue     *windows.LazyProc
	setValue     *windows.LazyProc
	getErrorText *windows.LazyProc
}

func loadPCANAPI() (*pcanAPI, error) {
	dll := windows.NewLazySystemDLL("PCANBasic.dll")
	if err := dll.Load(); err != nil {
		return nil, fmt.Errorf("%w: load PCANBasic.dll from the Windows system directory: %v",
			gocan.ErrDriverUnavailable, err)
	}

	api := &pcanAPI{
		initialize:   dll.NewProc("CAN_Initialize"),
		initializeFD: dll.NewProc("CAN_InitializeFD"),
		uninitialize: dll.NewProc("CAN_Uninitialize"),
		read:         dll.NewProc("CAN_Read"),
		readFD:       dll.NewProc("CAN_ReadFD"),
		write:        dll.NewProc("CAN_Write"),
		writeFD:      dll.NewProc("CAN_WriteFD"),
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
	var textBuffer [256]byte
	result, _, _ := api.getErrorText.Call(
		uintptr(status),
		0,
		uintptr(unsafe.Pointer(&textBuffer[0])),
	)
	if pcanStatus(result) != pcanStatusOK {
		return fmt.Errorf("%s: PCAN status %#08x", operation, uint32(status))
	}
	length := 0
	for length < len(textBuffer) && textBuffer[length] != 0 {
		length++
	}
	return fmt.Errorf("%s: PCAN status %#08x: %s", operation, uint32(status), textBuffer[:length])
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

// Bus is one open PCAN-Basic channel.
type Bus struct {
	id      gocan.BusID
	name    string
	capture *gocan.Capture
	channel Channel
	fd      bool
	api     *pcanAPI

	receiveEvent windows.Handle
	stopEvent    windows.Handle

	sendMu    sync.Mutex
	lifecycle *driverstate.Lifecycle

	deviceSubscription *devicechange.Subscription
	cleanupErr         error
}

var _ gocan.Bus = (*Bus)(nil)

// ID returns the one-based trace channel assigned to this bus.
func (bus *Bus) ID() gocan.BusID {
	return bus.id
}

// Name returns the human-readable name of this bus.
func (bus *Bus) Name() string {
	return bus.name
}

// Send hands frame to PCAN-Basic and records an accepted transmission.
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

	bus.sendMu.Lock()
	defer bus.sendMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bus.lifecycle.StopSignal():
		return bus.operationError()
	default:
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
		if status&(pcanStatusTransmitFull|pcanStatusQueueTXFull) != 0 {
			return fmt.Errorf("%w: PCAN status %#08x", gocan.ErrTransmitQueueFull, uint32(status))
		}
		err := bus.runtimeStatusError("send PCAN frame", status)
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

// Done is closed when this bus stops.
func (bus *Bus) Done() <-chan struct{} {
	return bus.lifecycle.Done()
}

// Err returns the background failure that stopped this bus, if any.
func (bus *Bus) Err() error {
	return bus.lifecycle.Err()
}

// Close stops acquisition and releases the native channel and event handles.
func (bus *Bus) Close() error {
	bus.stopWithError(nil)
	<-bus.lifecycle.Done()
	return bus.cleanupErr
}

func (bus *Bus) receiveLoop() {
	defer bus.finish()

	events := []windows.Handle{bus.stopEvent, bus.receiveEvent}
	for {
		event, err := windows.WaitForMultipleObjects(events, false, windows.INFINITE)
		if err != nil {
			bus.stopWithError(fmt.Errorf("wait for PCAN receive event: %w", err))
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
	for {
		select {
		case <-bus.lifecycle.StopSignal():
			return true, nil
		default:
		}

		// The device timestamp buffers stay NULL by design; received frames
		// are stamped with the host clock at read. See the package comment.
		var (
			observation pcanReceiveObservation
			timestamp   time.Time
			status      pcanStatus
			err         error
		)
		if bus.fd {
			var message pcanMsgFD
			result, _, _ := bus.api.readFD.Call(
				uintptr(bus.channel),
				uintptr(unsafe.Pointer(&message)),
				0,
			)
			timestamp = time.Now()
			status = pcanStatus(result)
			if status&(pcanStatusOverrun|pcanStatusQueueOverrun) != 0 {
				observation, err = decodePCANStatus(status, bus.id, timestamp)
			} else if status != pcanStatusQueueEmpty && status != pcanStatusInvalidData {
				observation, err = decodePCANReceive(
					message.id,
					message.messageType,
					message.dlc,
					message.data[:],
					true,
					status,
					bus.id,
					timestamp,
				)
			}
		} else {
			var message pcanMsg
			result, _, _ := bus.api.read.Call(
				uintptr(bus.channel),
				uintptr(unsafe.Pointer(&message)),
				0,
			)
			timestamp = time.Now()
			status = pcanStatus(result)
			if status&(pcanStatusOverrun|pcanStatusQueueOverrun) != 0 {
				observation, err = decodePCANStatus(status, bus.id, timestamp)
			} else if status != pcanStatusQueueEmpty && status != pcanStatusInvalidData {
				observation, err = decodePCANReceive(
					message.id,
					message.messageType,
					message.length,
					message.data[:],
					false,
					status,
					bus.id,
					timestamp,
				)
			}
		}

		if status&pcanStatusHandleMask == pcanStatusInvalidHW {
			return false, bus.runtimeStatusError("receive PCAN frame", status)
		}
		if status == pcanStatusQueueEmpty {
			return false, nil
		}
		// PCAN-Basic reports invalid wire data before delivering the associated
		// enabled error frame. The invalid data itself has already been discarded.
		if status == pcanStatusInvalidData {
			continue
		}
		if err != nil {
			return false, err
		}
		stopped, err := bus.applyObservation(observation, timestamp)
		if err != nil || stopped {
			return stopped, err
		}
	}
}

func (bus *Bus) applyObservation(observation pcanReceiveObservation, timestamp time.Time) (bool, error) {
	if observation.terminal != nil {
		bus.stopWithEvents(observation.terminal, observation.events[:observation.eventCount])
		return true, nil
	}
	for index := range observation.eventCount {
		if err := bus.capture.AppendEvent(observation.events[index]); err != nil {
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
	detachErr := bus.deviceSubscription.Cancel()
	bus.sendMu.Lock()
	bus.cleanupErr = errors.Join(detachErr, bus.cleanup())
	bus.sendMu.Unlock()
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
