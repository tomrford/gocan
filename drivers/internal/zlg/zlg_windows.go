//go:build windows

package zlg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/internal/devicechange"
	"github.com/tomrford/gocan/internal/driverstate"
	"golang.org/x/sys/windows"
)

// LibraryDirEnv names the directory holding zlgcan.dll and its kerneldlls
// folder. When unset, the executable's directory is used, matching ZLG's
// deployment layout.
const LibraryDirEnv = "GOCAN_ZLGCAN_DIR"

const usbcanInstancePrefix = `USB\VID_0471&PID_1200\`

// A ZLG USBCAN removal stops every open ZLG bus: device indices are assigned
// by the driver and cannot be mapped to one Windows device instance.
var processZLGDevices = devicechange.NewMonitor(usbcanInstancePrefix, "ZLG USBCAN")

const (
	receiveIdle         = time.Millisecond
	statePollTime       = 100 * time.Millisecond
	transmitQueueFrames = 256
	transmitBatchFrames = 16
)

type ChannelInfo struct {
	Device       uint32
	Channel      uint32
	HardwareType string
	Serial       string
}

// Discover reports the channels of attached USBCAN-I/II adapters. Probing
// opens each adapter briefly, so adapters held by another process are omitted;
// adapters open in this process are reported from their open handle.
func Discover() ([]ChannelInfo, error) {
	api, err := loadAPI()
	if err != nil {
		return nil, err
	}
	count, err := attachedUSBCANCount()
	if err != nil {
		return nil, err
	}
	devices.Lock()
	defer devices.Unlock()
	var channels []ChannelInfo
	for index := range count {
		info, ok := api.probeLocked(index)
		if !ok {
			continue
		}
		for channel := range uint32(info.channels) {
			channels = append(channels, ChannelInfo{
				Device:       index,
				Channel:      channel,
				HardwareType: cString(info.hardwareType[:]),
				Serial:       cString(info.serial[:]),
			})
		}
	}
	return channels, nil
}

// Open initializes a ZLG channel and starts capturing received frames.
// Context controls opening only; canceling it after Open returns does not stop
// the bus.
func Open(ctx context.Context, capture *gocan.Capture, config Config) (openedBus *Bus, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timing, ok := sja1000Timings[config.Bitrate]
	if !ok {
		return nil, fmt.Errorf("ZLG classical bitrate %d is not a ZLG predefined rate", config.Bitrate)
	}
	api, err := loadAPI()
	if err != nil {
		return nil, err
	}
	deviceHold, err := processZLGDevices.Hold()
	if err != nil {
		return nil, err
	}
	defer func() {
		if openedBus == nil {
			err = errors.Join(err, deviceHold.Release())
		}
	}()

	device, err := api.acquire(config.Device, config.Channel)
	if err != nil {
		return nil, err
	}
	init := initConfig{accMask: 0xffffffff, timing0: timing[0], timing1: timing[1]}
	channel, _, _ := api.initCAN.Call(device.handle, uintptr(config.Channel), uintptr(unsafe.Pointer(&init)))
	if channel == 0 {
		return nil, errors.Join(
			fmt.Errorf("initialize ZLG device %d channel %d", config.Device, config.Channel),
			api.release(config.Device, config.Channel),
		)
	}
	bus := &Bus{
		config:    config,
		capture:   capture,
		api:       api,
		channel:   channel,
		txQueue:   make(chan gocan.Frame, transmitQueueFrames),
		txDone:    make(chan struct{}),
		lifecycle: driverstate.New(nil),
	}
	for _, step := range []struct {
		proc      *windows.Proc
		operation string
	}{{api.startCAN, "start ZLG channel"}, {api.clearBuffer, "clear ZLG receive buffer"}} {
		if result, _, _ := step.proc.Call(channel); result != statusOK {
			return nil, errors.Join(fmt.Errorf("%s: status %d", step.operation, result), bus.cleanup())
		}
	}
	subscription, err := deviceHold.Bind(bus.lifecycle.Stop)
	if err != nil {
		return nil, errors.Join(err, bus.cleanup())
	}
	bus.deviceSubscription = subscription
	go bus.transmitLoop()
	go bus.receiveLoop()
	return bus, nil
}

type api struct {
	openDevice, closeDevice, deviceInfo               *windows.Proc
	initCAN, startCAN, resetCAN, clearBuffer          *windows.Proc
	transmit, receive, readErrorInfo, readChannelInfo *windows.Proc
}

var loadAPI = sync.OnceValues(func() (*api, error) {
	directory := os.Getenv(LibraryDirEnv)
	if directory == "" {
		executable, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate zlgcan.dll beside the executable: %w", err)
		}
		directory = filepath.Dir(executable)
	}
	path := filepath.Join(directory, "zlgcan.dll")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", gocan.ErrDriverUnavailable, path, err)
	}
	// The altered search path resolves zlgcan.dll's dependencies beside it
	// without changing the process DLL search path.
	module, err := windows.LoadLibraryEx(path, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	dll := &windows.DLL{Name: path, Handle: module}
	a := &api{}
	for _, procedure := range []struct {
		proc **windows.Proc
		name string
	}{
		{&a.openDevice, "ZCAN_OpenDevice"},
		{&a.closeDevice, "ZCAN_CloseDevice"},
		{&a.deviceInfo, "ZCAN_GetDeviceInf"},
		{&a.initCAN, "ZCAN_InitCAN"},
		{&a.startCAN, "ZCAN_StartCAN"},
		{&a.resetCAN, "ZCAN_ResetCAN"},
		{&a.clearBuffer, "ZCAN_ClearBuffer"},
		{&a.transmit, "ZCAN_Transmit"},
		{&a.receive, "ZCAN_Receive"},
		{&a.readErrorInfo, "ZCAN_ReadChannelErrInfo"},
		{&a.readChannelInfo, "ZCAN_ReadChannelStatus"},
	} {
		if *procedure.proc, err = dll.FindProc(procedure.name); err != nil {
			return nil, fmt.Errorf("resolve %s: %w", procedure.name, err)
		}
	}
	return a, nil
})

// zlgcan.dll admits one handle per device, which its channels share.
var devices = struct {
	sync.Mutex
	open map[uint32]*device
}{open: map[uint32]*device{}}

type device struct {
	handle   uintptr
	info     deviceInfo
	channels map[uint32]bool
}

func (api *api) acquire(index, channel uint32) (*device, error) {
	devices.Lock()
	defer devices.Unlock()
	opened := devices.open[index]
	if opened == nil {
		handle, _, _ := api.openDevice.Call(deviceTypeUSBCAN2, uintptr(index), 0)
		if handle == 0 {
			return nil, fmt.Errorf("open ZLG device %d: absent, in use by another process, or its kernel DLL failed to load", index)
		}
		opened = &device{handle: handle, channels: map[uint32]bool{}}
		if result, _, _ := api.deviceInfo.Call(handle, uintptr(unsafe.Pointer(&opened.info))); result != statusOK {
			_, _, _ = api.closeDevice.Call(handle)
			return nil, fmt.Errorf("read ZLG device %d information: status %d", index, result)
		}
		devices.open[index] = opened
	}
	switch {
	case channel >= uint32(opened.info.channels):
		err := fmt.Errorf("ZLG device %d has %d channels; channel %d does not exist", index, opened.info.channels, channel)
		return nil, errors.Join(err, api.releaseLocked(index, channel))
	case opened.channels[channel]:
		return nil, fmt.Errorf("ZLG device %d channel %d is already open", index, channel)
	}
	opened.channels[channel] = true
	return opened, nil
}

func (api *api) release(index, channel uint32) error {
	devices.Lock()
	defer devices.Unlock()
	return api.releaseLocked(index, channel)
}

func (api *api) releaseLocked(index, channel uint32) error {
	opened := devices.open[index]
	delete(opened.channels, channel)
	if len(opened.channels) != 0 {
		return nil
	}
	delete(devices.open, index)
	if result, _, _ := api.closeDevice.Call(opened.handle); result != statusOK {
		return fmt.Errorf("close ZLG device %d: status %d", index, result)
	}
	return nil
}

func (api *api) probeLocked(index uint32) (deviceInfo, bool) {
	if opened := devices.open[index]; opened != nil {
		return opened.info, true
	}
	handle, _, _ := api.openDevice.Call(deviceTypeUSBCAN2, uintptr(index), 0)
	if handle == 0 {
		return deviceInfo{}, false
	}
	defer api.closeDevice.Call(handle)
	var info deviceInfo
	result, _, _ := api.deviceInfo.Call(handle, uintptr(unsafe.Pointer(&info)))
	return info, result == statusOK
}

func attachedUSBCANCount() (uint32, error) {
	set, err := windows.SetupDiGetClassDevsEx(nil, "USB", 0, windows.DIGCF_PRESENT|windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		return 0, fmt.Errorf("enumerate USB devices: %w", err)
	}
	defer set.Close()
	var count uint32
	for index := 0; ; index++ {
		data, err := set.EnumDeviceInfo(index)
		if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
			return count, nil
		}
		if err != nil {
			return 0, fmt.Errorf("enumerate USB devices: %w", err)
		}
		id, err := set.DeviceInstanceID(data)
		if err != nil {
			return 0, fmt.Errorf("read USB device instance: %w", err)
		}
		if strings.HasPrefix(strings.ToUpper(id), usbcanInstancePrefix) {
			count++
		}
	}
}

func cString(value []byte) string {
	if end := bytes.IndexByte(value, 0); end >= 0 {
		value = value[:end]
	}
	return string(value)
}

type Bus struct {
	config  Config
	capture *gocan.Capture
	api     *api
	channel uintptr

	txMu      sync.Mutex
	txQueue   chan gocan.Frame
	txClosed  bool
	txDone    chan struct{}
	lifecycle *driverstate.Lifecycle
	lastState gocan.Event

	deviceSubscription *devicechange.Subscription
	cleanupErr         error
}

var _ gocan.Bus = (*Bus)(nil)

func (bus *Bus) ID() gocan.BusID         { return bus.config.ID }
func (bus *Bus) Name() string            { return bus.config.Name }
func (bus *Bus) Capture() *gocan.Capture { return bus.capture }
func (bus *Bus) Done() <-chan struct{}   { return bus.lifecycle.Done() }
func (bus *Bus) Err() error              { return bus.lifecycle.Err() }

// Close transmits frames already accepted by Send, stopping early if a
// transmission fails, then releases the channel.
func (bus *Bus) Close() error {
	bus.lifecycle.Stop(nil)
	<-bus.lifecycle.Done()
	return bus.cleanupErr
}

// Send records frame and queues it for the transmit goroutine without waiting
// for the adapter. It returns ErrTransmitQueueFull while transmitQueueFrames
// frames are pending. A queued frame the adapter fails to transmit is dropped
// and reported as an error-frame event.
func (bus *Bus) Send(ctx context.Context, frame gocan.Frame) error {
	if err := validateSendFrame(frame); err != nil {
		return err
	}
	if err := bus.sendable(ctx); err != nil {
		return err
	}
	bus.txMu.Lock()
	defer bus.txMu.Unlock()
	if err := bus.sendable(ctx); err != nil {
		return err
	}
	if bus.txClosed {
		return bus.lifecycle.OperationError()
	}
	if len(bus.txQueue) == cap(bus.txQueue) {
		return fmt.Errorf("%w: ZLG transmit queue holds %d frames", gocan.ErrTransmitQueueFull, cap(bus.txQueue))
	}
	// Recording before queueing keeps capture order equal to wire order and
	// places each transmission before any response to it.
	if err := bus.capture.RecordFrame(gocan.FrameEvent{Bus: bus.ID(), Direction: gocan.DirectionTransmit, Frame: frame}); err != nil {
		bus.lifecycle.Stop(err)
		return err
	}
	bus.txQueue <- frame
	return nil
}

func (bus *Bus) sendable(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bus.lifecycle.StopSignal():
		return bus.lifecycle.OperationError()
	default:
		return nil
	}
}

// transmitLoop passes every pending frame to the adapter in one call: each
// ZCAN_Transmit call waits for its frames to leave the adapter, about 1 ms per
// call plus 0.45 ms per frame on a USBCAN-I (October 2026).
func (bus *Bus) transmitLoop() {
	defer close(bus.txDone)
	batch := make([]transmitData, 0, transmitBatchFrames)
	for {
		select {
		case frame := <-bus.txQueue:
			batch = append(batch[:0], transmitData{frame: encodeFrame(frame)})
		case <-bus.lifecycle.StopSignal():
			bus.drain(batch)
			return
		}
		batch = bus.fillBatch(batch)
		if _, err := bus.transmit(batch); err != nil {
			bus.lifecycle.Stop(err)
			return
		}
	}
}

func (bus *Bus) fillBatch(batch []transmitData) []transmitData {
	for len(batch) < cap(batch) {
		select {
		case frame := <-bus.txQueue:
			batch = append(batch, transmitData{frame: encodeFrame(frame)})
		default:
			return batch
		}
	}
	return batch
}

// drain transmits the frames accepted before a normal Close.
func (bus *Bus) drain(batch []transmitData) {
	bus.txMu.Lock()
	bus.txClosed = true
	bus.txMu.Unlock()
	if bus.lifecycle.Err() != nil {
		return
	}
	for len(bus.txQueue) != 0 {
		batch = bus.fillBatch(batch[:0])
		if failed, err := bus.transmit(batch); failed || err != nil {
			return
		}
	}
}

// transmit sends batch, dropping each frame the adapter fails to send. The
// adapter abandons the rest of a call after a failure, so they are resent.
func (bus *Bus) transmit(batch []transmitData) (bool, error) {
	failed := false
	for len(batch) != 0 {
		sent, _, _ := bus.api.transmit.Call(bus.channel, uintptr(unsafe.Pointer(&batch[0])), uintptr(len(batch)))
		if int(sent) >= len(batch) {
			break
		}
		failed = true
		if err := bus.capture.RecordEvent(gocan.Event{Bus: bus.ID(), Kind: gocan.EventErrorFrame}); err != nil {
			return failed, err
		}
		batch = batch[sent+1:]
	}
	return failed, nil
}

func (bus *Bus) receiveLoop() {
	defer bus.finish()
	idle := time.NewTimer(time.Hour)
	idle.Stop()
	defer idle.Stop()
	nextState := time.Now()
	for {
		select {
		case <-bus.lifecycle.StopSignal():
			return
		default:
		}
		more, err := bus.receiveOne()
		if err == nil && !time.Now().Before(nextState) {
			err = bus.checkState()
			nextState = time.Now().Add(statePollTime)
		}
		if err != nil {
			bus.lifecycle.Stop(err)
			return
		}
		if more {
			continue
		}
		idle.Reset(receiveIdle)
		select {
		case <-bus.lifecycle.StopSignal():
			return
		case <-idle.C:
		}
	}
}

func (bus *Bus) receiveOne() (bool, error) {
	var native receiveData
	if received, _, _ := bus.api.receive.Call(bus.channel, uintptr(unsafe.Pointer(&native)), 1, 0); received == 0 {
		return false, nil
	}
	frame, err := decodeFrame(native.frame)
	if err != nil {
		return false, err
	}
	return true, bus.capture.RecordFrame(gocan.FrameEvent{Bus: bus.ID(), Direction: gocan.DirectionReceive, Frame: frame})
}

func (bus *Bus) checkState() error {
	var errorInfo channelErrorInfo
	if result, _, _ := bus.api.readErrorInfo.Call(bus.channel, uintptr(unsafe.Pointer(&errorInfo))); result != statusOK {
		return fmt.Errorf("read ZLG channel error information: status %d", result)
	}
	var status channelStatus
	if result, _, _ := bus.api.readChannelInfo.Call(bus.channel, uintptr(unsafe.Pointer(&status))); result != statusOK {
		return fmt.Errorf("read ZLG channel status: status %d", result)
	}
	event, terminal := controllerEvent(bus.ID(), status, errorInfo.code)
	if event != bus.lastState || terminal != nil {
		if err := bus.capture.RecordEvent(event); err != nil {
			return err
		}
		bus.lastState = event
	}
	return terminal
}

// finish waits for the transmit goroutine, whose native call cannot be
// interrupted, before resetting the channel.
func (bus *Bus) finish() {
	<-bus.txDone
	detachErr := bus.deviceSubscription.Cancel()
	bus.cleanupErr = errors.Join(detachErr, bus.cleanup())
	bus.lifecycle.MarkDone()
}

func (bus *Bus) cleanup() error {
	var cleanupErr error
	if result, _, _ := bus.api.resetCAN.Call(bus.channel); result != statusOK {
		cleanupErr = fmt.Errorf("reset ZLG channel: status %d", result)
	}
	return errors.Join(cleanupErr, bus.api.release(bus.config.Device, bus.config.Channel))
}
