//go:build windows

package zlg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/internal/devicechange"
	"github.com/tomrford/gocan/internal/driverstate"
	"golang.org/x/sys/windows"
)

// A family is a set of adapters sharing a kernel DLL. USBCANFD-100U, 200U and
// MINI share USB IDs, so Discover tries each of their device types.
type family struct {
	instancePrefixes []string
	deviceTypes      []uint32
}

var families = [...]family{
	{[]string{`USB\VID_0471&PID_1200\`}, []uint32{deviceTypeUSBCAN2}},
	{[]string{`USB\VID_3068&PID_0009\`, `USB\VID_04CC&PID_1240\`}, []uint32{deviceTypeUSBCANFD200U, deviceTypeUSBCANFD100U, deviceTypeUSBCANFDMini}},
}

// A ZLG adapter removal stops every open ZLG bus: device indices are assigned
// by the driver and cannot be mapped to one Windows device instance.
var processZLGDevices = devicechange.NewMonitor("ZLG USB",
	slices.Concat(families[0].instancePrefixes, families[1].instancePrefixes)...)

// USBCAN.dll returned a record misaligned by one byte when asked for 64 frames
// per call under bidirectional load (November 2026), so each receive call reads
// one frame; a buffered read costs about 1 µs.
const (
	receiveIdle         = time.Millisecond
	statePollTime       = 100 * time.Millisecond
	transmitQueueFrames = 256
	transmitBatchFrames = 16
	receiveBatchFrames  = 1
)

type ChannelInfo struct {
	DeviceType   uint32
	Device       uint32
	Channel      uint32
	HardwareType string
	Serial       string
	// SupportsFD also reports switchable termination.
	SupportsFD bool
}

// Discover reports the channels of attached USBCAN-I/II and USBCANFD
// adapters. Probing opens each adapter briefly, so adapters held by another
// process are omitted; adapters open in this process are reported from their
// open handle. ZLG's libraries are unpacked and loaded only once an adapter is
// attached.
func Discover() ([]ChannelInfo, error) {
	counts, err := attachedCounts()
	if err != nil {
		return nil, err
	}
	if counts == [len(families)]uint32{} {
		return nil, nil
	}
	api, err := loadAPI()
	if err != nil {
		return nil, err
	}
	devices.Lock()
	defer devices.Unlock()
	var channels []ChannelInfo
	for familyIndex, family := range families {
		seen := map[string]bool{}
		for _, deviceType := range family.deviceTypes {
			for index := range counts[familyIndex] {
				// Closing a probe of another type could release a handle
				// this process holds if the DLL tracks devices by index alone.
				if slices.ContainsFunc(family.deviceTypes, func(openType uint32) bool {
					return openType != deviceType && devices.open[deviceKey{openType, index}] != nil
				}) {
					continue
				}
				info, ok := api.probeLocked(deviceKey{deviceType, index})
				serial := cString(info.serial[:])
				if !ok || serial != "" && seen[serial] {
					continue
				}
				seen[serial] = true
				for channel := range uint32(info.channels) {
					channels = append(channels, ChannelInfo{
						DeviceType:   deviceType,
						Device:       index,
						Channel:      channel,
						HardwareType: cString(info.hardwareType[:]),
						Serial:       serial,
						SupportsFD:   isFDDevice(deviceType),
					})
				}
			}
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
	if err := validateConfig(config); err != nil {
		return nil, err
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

	key := deviceKey{config.DeviceType, config.Device}
	device, err := api.acquire(key, config.Channel)
	if err != nil {
		return nil, err
	}
	channel, err := api.initChannel(device, config)
	if err != nil {
		return nil, errors.Join(err, api.release(key, config.Channel))
	}
	bus := &Bus{
		config:    config,
		capture:   capture,
		api:       api,
		channel:   channel,
		fdDevice:  isFDDevice(config.DeviceType),
		fd:        config.DataBitrate != 0,
		txQueue:   make(chan gocan.Frame, transmitQueueFrames),
		txDone:    make(chan struct{}),
		lifecycle: driverstate.New(nil),
	}
	if config.Termination != 0 {
		// ZLG applies termination after InitCAN and spells the property this way.
		value := strconv.Itoa(int(config.Termination - 1))
		if err := api.setValue(device.handle, fmt.Sprintf("%d/initenal_resistance", config.Channel), value); err != nil {
			return nil, errors.Join(err, bus.cleanup())
		}
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

// initChannel configures and initializes a channel. USBCANFD adapters take
// their rates as properties set before ZCAN_InitCAN, and always initialize as
// ISO CAN FD controllers; a classical bus sets the data rate to 1 Mbit/s, at
// or above every nominal rate.
func (api *api) initChannel(device *device, config Config) (uintptr, error) {
	var init unsafe.Pointer
	if isFDDevice(config.DeviceType) {
		dataBitrate := config.DataBitrate
		if dataBitrate == 0 {
			dataBitrate = 1_000_000
		}
		for _, property := range [...]struct{ name, value string }{
			{"canfd_standard", "0"},
			{"canfd_abit_baud_rate", strconv.FormatUint(uint64(config.Bitrate), 10)},
			{"canfd_dbit_baud_rate", strconv.FormatUint(uint64(dataBitrate), 10)},
		} {
			if err := api.setValue(device.handle, fmt.Sprintf("%d/%s", config.Channel, property.name), property.value); err != nil {
				return 0, err
			}
		}
		init = unsafe.Pointer(&initFDConfig{canType: canTypeFD, accMask: 0xffffffff})
	} else {
		timing := sja1000Timings[config.Bitrate]
		init = unsafe.Pointer(&initConfig{accMask: 0xffffffff, timing0: timing[0], timing1: timing[1]})
	}
	channel, _, _ := api.initCAN.Call(device.handle, uintptr(config.Channel), uintptr(init))
	if channel == 0 {
		return 0, fmt.Errorf("initialize ZLG device type %d index %d channel %d", config.DeviceType, config.Device, config.Channel)
	}
	return channel, nil
}

type api struct {
	openDevice, closeDevice, deviceInfo, setValueProc *windows.Proc
	initCAN, startCAN, resetCAN, clearBuffer          *windows.Proc
	transmit, receive, transmitFD, receiveFD          *windows.Proc
	readErrorInfo, readChannelInfo                    *windows.Proc
}

// loadAPI unpacks and loads the bundled zlgcan.dll once per process. A load
// failure, usually a missing Visual C++ 2008 runtime, reports the driver
// unavailable.
var loadAPI = sync.OnceValues(func() (*api, error) {
	directory, err := unpackBundle()
	if err != nil {
		return nil, fmt.Errorf("%w: unpack ZLG libraries: %v", gocan.ErrDriverUnavailable, err)
	}
	path := filepath.Join(directory, "zlgcan.dll")
	// The altered search path resolves zlgcan.dll's dependencies beside it
	// without changing the process DLL search path.
	module, err := windows.LoadLibraryEx(path, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH)
	if err != nil {
		return nil, fmt.Errorf("%w: load %s (requires the Visual C++ 2008 SP1 x64 redistributable): %v", gocan.ErrDriverUnavailable, path, err)
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
		{&a.setValueProc, "ZCAN_SetValue"},
		{&a.initCAN, "ZCAN_InitCAN"},
		{&a.startCAN, "ZCAN_StartCAN"},
		{&a.resetCAN, "ZCAN_ResetCAN"},
		{&a.clearBuffer, "ZCAN_ClearBuffer"},
		{&a.transmit, "ZCAN_Transmit"},
		{&a.receive, "ZCAN_Receive"},
		{&a.transmitFD, "ZCAN_TransmitFD"},
		{&a.receiveFD, "ZCAN_ReceiveFD"},
		{&a.readErrorInfo, "ZCAN_ReadChannelErrInfo"},
		{&a.readChannelInfo, "ZCAN_ReadChannelStatus"},
	} {
		if *procedure.proc, err = dll.FindProc(procedure.name); err != nil {
			return nil, fmt.Errorf("resolve %s: %w", procedure.name, err)
		}
	}
	return a, nil
})

func (api *api) setValue(handle uintptr, path, value string) error {
	nativePath, err := windows.BytePtrFromString(path)
	if err != nil {
		return err
	}
	nativeValue, err := windows.BytePtrFromString(value)
	if err != nil {
		return err
	}
	if result, _, _ := api.setValueProc.Call(handle, uintptr(unsafe.Pointer(nativePath)), uintptr(unsafe.Pointer(nativeValue))); result != statusOK {
		return fmt.Errorf("set ZLG property %s to %s: status %d", path, value, result)
	}
	return nil
}

type deviceKey struct {
	deviceType uint32
	index      uint32
}

// zlgcan.dll admits one handle per device, which its channels share.
var devices = struct {
	sync.Mutex
	open map[deviceKey]*device
}{open: map[deviceKey]*device{}}

type device struct {
	handle   uintptr
	info     deviceInfo
	channels map[uint32]bool
}

func (api *api) acquire(key deviceKey, channel uint32) (*device, error) {
	devices.Lock()
	defer devices.Unlock()
	opened := devices.open[key]
	if opened == nil {
		handle, _, _ := api.openDevice.Call(uintptr(key.deviceType), uintptr(key.index), 0)
		if handle == 0 {
			return nil, fmt.Errorf("open ZLG device type %d index %d: absent, in use by another process, or its kernel DLL failed to load (USBCANFD needs the Visual C++ 2013 x64 redistributable)", key.deviceType, key.index)
		}
		opened = &device{handle: handle, channels: map[uint32]bool{}}
		if result, _, _ := api.deviceInfo.Call(handle, uintptr(unsafe.Pointer(&opened.info))); result != statusOK {
			_, _, _ = api.closeDevice.Call(handle)
			return nil, fmt.Errorf("read ZLG device type %d index %d information: status %d", key.deviceType, key.index, result)
		}
		devices.open[key] = opened
	}
	switch {
	case channel >= uint32(opened.info.channels):
		err := fmt.Errorf("ZLG device type %d index %d has %d channels; channel %d does not exist", key.deviceType, key.index, opened.info.channels, channel)
		return nil, errors.Join(err, api.releaseLocked(key, channel))
	case opened.channels[channel]:
		return nil, fmt.Errorf("ZLG device type %d index %d channel %d is already open", key.deviceType, key.index, channel)
	}
	opened.channels[channel] = true
	return opened, nil
}

func (api *api) release(key deviceKey, channel uint32) error {
	devices.Lock()
	defer devices.Unlock()
	return api.releaseLocked(key, channel)
}

func (api *api) releaseLocked(key deviceKey, channel uint32) error {
	opened := devices.open[key]
	delete(opened.channels, channel)
	if len(opened.channels) != 0 {
		return nil
	}
	delete(devices.open, key)
	if result, _, _ := api.closeDevice.Call(opened.handle); result != statusOK {
		return fmt.Errorf("close ZLG device type %d index %d: status %d", key.deviceType, key.index, result)
	}
	return nil
}

func (api *api) probeLocked(key deviceKey) (deviceInfo, bool) {
	if opened := devices.open[key]; opened != nil {
		return opened.info, true
	}
	handle, _, _ := api.openDevice.Call(uintptr(key.deviceType), uintptr(key.index), 0)
	if handle == 0 {
		return deviceInfo{}, false
	}
	defer api.closeDevice.Call(handle)
	var info deviceInfo
	result, _, _ := api.deviceInfo.Call(handle, uintptr(unsafe.Pointer(&info)))
	return info, result == statusOK
}

// attachedCounts returns the number of present USB devices in each family.
func attachedCounts() ([len(families)]uint32, error) {
	var counts [len(families)]uint32
	set, err := windows.SetupDiGetClassDevsEx(nil, "USB", 0, windows.DIGCF_PRESENT|windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		return counts, fmt.Errorf("enumerate USB devices: %w", err)
	}
	defer set.Close()
	for index := 0; ; index++ {
		data, err := set.EnumDeviceInfo(index)
		if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
			return counts, nil
		}
		if err != nil {
			return counts, fmt.Errorf("enumerate USB devices: %w", err)
		}
		id, err := set.DeviceInstanceID(data)
		if err != nil {
			return counts, fmt.Errorf("read USB device instance: %w", err)
		}
		id = strings.ToUpper(id)
		for familyIndex, family := range families {
			for _, prefix := range family.instancePrefixes {
				if strings.HasPrefix(id, prefix) {
					counts[familyIndex]++
				}
			}
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
	config   Config
	capture  *gocan.Capture
	api      *api
	channel  uintptr
	fdDevice bool
	fd       bool

	txMu      sync.Mutex
	txQueue   chan gocan.Frame
	txClosed  bool
	txDone    chan struct{}
	lifecycle *driverstate.Lifecycle
	lastState gocan.Event

	// Native buffers owned by the transmit and receive goroutines.
	txFrames   [transmitBatchFrames]transmitData
	txFDFrames [transmitBatchFrames]transmitFDData
	rxFrames   [receiveBatchFrames]receiveData
	rxFDFrames [receiveBatchFrames]receiveFDData

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
// frames are pending. A queued frame the adapter does not confirm is dropped
// and reported as an error-frame event. Frames still queued when the bus stops
// with an error, or after a failed transmission during Close, are discarded
// without an event.
func (bus *Bus) Send(ctx context.Context, frame gocan.Frame) error {
	if err := validateSendFrame(frame, bus.fd); err != nil {
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

// transmitLoop passes pending frames to the adapter in as few calls as
// possible: each ZCAN_Transmit call on a USBCAN-I waits for its frames to
// leave the adapter, about 1 ms per call plus 0.45 ms per frame (October 2026).
func (bus *Bus) transmitLoop() {
	defer close(bus.txDone)
	batch := make([]gocan.Frame, 0, transmitBatchFrames)
	for {
		select {
		case frame := <-bus.txQueue:
			batch = append(batch[:0], frame)
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

func (bus *Bus) fillBatch(batch []gocan.Frame) []gocan.Frame {
	for len(batch) < cap(batch) {
		select {
		case frame := <-bus.txQueue:
			batch = append(batch, frame)
		default:
			return batch
		}
	}
	return batch
}

// drain transmits the frames accepted before a normal Close.
func (bus *Bus) drain(batch []gocan.Frame) {
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

// transmit sends batch in runs of classical or CAN FD frames. When a call
// reports fewer frames sent than requested, every unconfirmed frame of that
// run is dropped and reported as an error-frame event, never resent: USBCAN.dll
// has under-reported a frame that reached the wire, so a resend could
// duplicate it. A failing call costs the adapter's one-second timeout.
func (bus *Bus) transmit(batch []gocan.Frame) (bool, error) {
	failed := false
	for len(batch) != 0 {
		fd := batch[0].Flags.Has(gocan.FrameFD)
		run := 1
		for run < len(batch) && batch[run].Flags.Has(gocan.FrameFD) == fd {
			run++
		}
		for range run - min(bus.transmitRun(batch[:run], fd), run) {
			failed = true
			if err := bus.capture.RecordEvent(gocan.Event{Bus: bus.ID(), Kind: gocan.EventErrorFrame}); err != nil {
				return failed, err
			}
		}
		batch = batch[run:]
	}
	return failed, nil
}

func (bus *Bus) transmitRun(frames []gocan.Frame, fd bool) int {
	var sent uintptr
	if fd {
		for index, frame := range frames {
			bus.txFDFrames[index] = transmitFDData{frame: encodeFDFrame(frame)}
		}
		sent, _, _ = bus.api.transmitFD.Call(bus.channel, uintptr(unsafe.Pointer(&bus.txFDFrames[0])), uintptr(len(frames)))
	} else {
		for index, frame := range frames {
			bus.txFrames[index] = transmitData{frame: encodeFrame(frame)}
		}
		sent, _, _ = bus.api.transmit.Call(bus.channel, uintptr(unsafe.Pointer(&bus.txFrames[0])), uintptr(len(frames)))
	}
	return int(sent)
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
		more, err := bus.receiveBatch()
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

// receiveBatch reads classical and, on USBCANFD adapters, CAN FD frames from
// their separate native queues and records them in native timestamp order.
func (bus *Bus) receiveBatch() (bool, error) {
	received, _, _ := bus.api.receive.Call(bus.channel, uintptr(unsafe.Pointer(&bus.rxFrames[0])), receiveBatchFrames, 0)
	var receivedFD uintptr
	if bus.fdDevice {
		receivedFD, _, _ = bus.api.receiveFD.Call(bus.channel, uintptr(unsafe.Pointer(&bus.rxFDFrames[0])), receiveBatchFrames, 0)
	}
	received, receivedFD = min(received, receiveBatchFrames), min(receivedFD, receiveBatchFrames)
	classic, fd := bus.rxFrames[:received], bus.rxFDFrames[:receivedFD]
	for len(classic) != 0 || len(fd) != 0 {
		var frame gocan.Frame
		var err error
		if len(fd) == 0 || len(classic) != 0 && classic[0].timestamp <= fd[0].timestamp {
			frame, err = decodeFrame(classic[0].frame)
			classic = classic[1:]
		} else {
			frame, err = decodeFDFrame(fd[0].frame)
			fd = fd[1:]
		}
		switch {
		case errors.Is(err, errErrorFrame):
			err = bus.capture.RecordEvent(gocan.Event{Bus: bus.ID(), Kind: gocan.EventErrorFrame})
		case err == nil:
			err = bus.capture.RecordFrame(gocan.FrameEvent{Bus: bus.ID(), Direction: gocan.DirectionReceive, Frame: frame})
		}
		if err != nil {
			return false, err
		}
	}
	return received == receiveBatchFrames || receivedFD == receiveBatchFrames, nil
}

// checkState polls controller state. USBCANFD state reads are unverified on
// hardware, so a failed read there skips the poll instead of stopping the bus.
func (bus *Bus) checkState() error {
	var errorInfo channelErrorInfo
	if result, _, _ := bus.api.readErrorInfo.Call(bus.channel, uintptr(unsafe.Pointer(&errorInfo))); result != statusOK {
		if bus.fdDevice {
			return nil
		}
		return fmt.Errorf("read ZLG channel error information: status %d", result)
	}
	status := &channelStatus{}
	if result, _, _ := bus.api.readChannelInfo.Call(bus.channel, uintptr(unsafe.Pointer(status))); result != statusOK {
		if !bus.fdDevice {
			return fmt.Errorf("read ZLG channel status: status %d", result)
		}
		status = nil
	}
	event, terminal := controllerEvent(bus.ID(), status, errorInfo.code, !bus.fdDevice)
	if event.Kind != 0 && (event != bus.lastState || terminal != nil) {
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
	return errors.Join(cleanupErr, bus.api.release(deviceKey{bus.config.DeviceType, bus.config.Device}, bus.config.Channel))
}
