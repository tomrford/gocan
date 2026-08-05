//go:build windows

// Package devicechange reports Windows Plug and Play device-instance changes
// and fans matching removals out to open buses; see Monitor.
package devicechange

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Action identifies a Windows device-instance transition.
type Action uint32

const (
	ActionInstanceEnumerated Action = 7
	ActionInstanceStarted    Action = 8
	ActionInstanceRemoved    Action = 9
)

const (
	cmNotifyFilterAllDeviceInstances = 0x00000002
	cmNotifyFilterDeviceInstance     = 2
	maxDeviceIDLength                = 200
	watcherBuffer                    = 256
)

var (
	cfgMgr32                 = windows.NewLazySystemDLL("cfgmgr32.dll")
	cmRegisterNotification   = cfgMgr32.NewProc("CM_Register_Notification")
	cmUnregisterNotification = cfgMgr32.NewProc("CM_Unregister_Notification")
)

// Event is one observed removal of a device instance matching the watcher's
// prefix.
type Event struct {
	InstanceID string
}

type cmNotifyFilter struct {
	Size       uint32
	Flags      uint32
	FilterType uint32
	Reserved   uint32
	InstanceID [maxDeviceIDLength]uint16
}

// The Go runtime never releases a callback trampoline and caps them
// process-wide, so one shared callback serves every Watcher ever registered.
// Each registration finds its Watcher through the context value passed to
// CM_Register_Notification.
var (
	notifyCallback = sync.OnceValue(func() uintptr {
		return windows.NewCallback(dispatchNotification)
	})
	watcherContexts    sync.Map // uintptr → *Watcher
	nextWatcherContext atomic.Uintptr
)

func dispatchNotification(
	_ uintptr,
	contextID uintptr,
	action uintptr,
	eventData unsafe.Pointer,
	eventDataSize uintptr,
) uintptr {
	if value, ok := watcherContexts.Load(contextID); ok {
		value.(*Watcher).notify(Action(action), eventData, eventDataSize)
	}
	return 0
}

// Watcher observes removals of device instances whose IDs match one prefix.
// The Windows registration still sees every device-instance transition in the
// session, but only matching removals enter the finite Events queue, so
// unrelated Plug and Play churn cannot overrun it.
type Watcher struct {
	handle    uintptr
	contextID uintptr
	prefix    string
	events    chan Event
	lost      atomic.Bool

	closeOnce sync.Once
	closeErr  error
}

// WatchRemovals registers for removals of Windows device instances whose IDs
// start with prefix, compared case-insensitively.
func WatchRemovals(prefix string) (*Watcher, error) {
	watcher := &Watcher{
		events:    make(chan Event, watcherBuffer),
		contextID: nextWatcherContext.Add(1),
		prefix:    prefix,
	}
	watcherContexts.Store(watcher.contextID, watcher)

	filter := cmNotifyFilter{
		Size:       uint32(unsafe.Sizeof(cmNotifyFilter{})),
		Flags:      cmNotifyFilterAllDeviceInstances,
		FilterType: cmNotifyFilterDeviceInstance,
	}
	result, _, _ := cmRegisterNotification.Call(
		uintptr(unsafe.Pointer(&filter)),
		watcher.contextID,
		notifyCallback(),
		uintptr(unsafe.Pointer(&watcher.handle)),
	)
	if result != 0 {
		watcherContexts.Delete(watcher.contextID)
		return nil, fmt.Errorf("register Windows device-instance notifications: CONFIGRET %#x", uint32(result))
	}
	return watcher, nil
}

// Events returns the stream of observed matching removals.
func (watcher *Watcher) Events() <-chan Event { return watcher.events }

// Lost reports and clears whether the callback dropped an event because the
// consumer did not drain Events quickly enough.
func (watcher *Watcher) Lost() bool { return watcher.lost.Swap(false) }

// Close releases the Windows notification registration. Events is deliberately
// left open so a callback already in flight can finish without racing a close.
func (watcher *Watcher) Close() error {
	watcher.closeOnce.Do(func() {
		result, _, _ := cmUnregisterNotification.Call(watcher.handle)
		// CM_Unregister_Notification returns after in-flight callbacks
		// complete, so the context entry can be dropped without racing one.
		watcherContexts.Delete(watcher.contextID)
		if result != 0 {
			watcher.closeErr = fmt.Errorf(
				"unregister Windows device-instance notifications: CONFIGRET %#x",
				uint32(result),
			)
		}
	})
	return watcher.closeErr
}

func (watcher *Watcher) notify(action Action, eventData unsafe.Pointer, eventDataSize uintptr) {
	if action != ActionInstanceRemoved {
		return
	}
	instanceID, ok := decodeInstanceID(eventData, eventDataSize)
	if !ok || !matchesPrefix(instanceID, watcher.prefix) {
		return
	}
	select {
	case watcher.events <- Event{InstanceID: instanceID}:
	default:
		watcher.lost.Store(true)
	}
}

func matchesPrefix(instanceID, prefix string) bool {
	return len(instanceID) >= len(prefix) && strings.EqualFold(instanceID[:len(prefix)], prefix)
}

func decodeInstanceID(eventData unsafe.Pointer, eventDataSize uintptr) (string, bool) {
	const instanceIDOffset = 8
	if eventData == nil || eventDataSize < instanceIDOffset+2 {
		return "", false
	}
	if *(*uint32)(eventData) != cmNotifyFilterDeviceInstance {
		return "", false
	}
	length := (eventDataSize - instanceIDOffset) / 2
	value := unsafe.Slice((*uint16)(unsafe.Add(eventData, instanceIDOffset)), length)
	for index, character := range value {
		if character == 0 {
			value = value[:index]
			break
		}
	}
	if len(value) == 0 {
		return "", false
	}
	return windows.UTF16ToString(value), true
}
