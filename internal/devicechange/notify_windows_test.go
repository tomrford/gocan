//go:build windows

package devicechange

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestNotifyFilterNativeLayout(t *testing.T) {
	if got := unsafe.Sizeof(cmNotifyFilter{}); got != 416 {
		t.Fatalf("CM_NOTIFY_FILTER size = %d, want 416", got)
	}
	if got := unsafe.Offsetof(cmNotifyFilter{}.InstanceID); got != 16 {
		t.Fatalf("CM_NOTIFY_FILTER union offset = %d, want 16", got)
	}
}

// encodeInstanceEventData builds a synthetic CM_NOTIFY_EVENT_DATA buffer for
// one device-instance identifier.
func encodeInstanceEventData(t *testing.T, identifier string) []byte {
	t.Helper()
	encoded, err := windows.UTF16FromString(identifier)
	if err != nil {
		t.Fatalf("UTF16FromString: %v", err)
	}
	buffer := make([]byte, 8+2*len(encoded))
	binary.LittleEndian.PutUint32(buffer, cmNotifyFilterDeviceInstance)
	for index, character := range encoded {
		binary.LittleEndian.PutUint16(buffer[8+2*index:], character)
	}
	return buffer
}

func TestDecodeInstanceID(t *testing.T) {
	identifier := `USB\VID_0C72&PID_000C\example`
	buffer := encodeInstanceEventData(t, identifier)

	got, ok := decodeInstanceID(
		unsafe.Pointer(&buffer[0]),
		uintptr(len(buffer)),
	)
	if !ok || got != identifier {
		t.Fatalf("decodeInstanceID = %q, %t; want %q, true", got, ok, identifier)
	}
	buffer[0] = 1
	if _, ok := decodeInstanceID(unsafe.Pointer(&buffer[0]), uintptr(len(buffer))); ok {
		t.Fatal("decodeInstanceID accepted device-interface event data")
	}
}

func TestWatchRemovalsRegisters(t *testing.T) {
	watcher, err := WatchRemovals(`USB\VID_0C72&`)
	if err != nil {
		t.Fatalf("WatchRemovals: %v", err)
	}
	if _, ok := watcherContexts.Load(watcher.contextID); !ok {
		t.Fatal("registration did not enter the dispatch registry")
	}
	if err := watcher.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := watcherContexts.Load(watcher.contextID); ok {
		t.Fatal("Close left the watcher in the dispatch registry")
	}
}

// newUnregisteredTestWatcher enters the dispatch registry without a Windows
// registration, so dispatchNotification can be driven directly and no real
// PnP event can reach the watcher.
func newUnregisteredTestWatcher(t *testing.T) *Watcher {
	t.Helper()
	watcher := &Watcher{
		events:    make(chan Event, watcherBuffer),
		contextID: nextWatcherContext.Add(1),
		prefix:    `USB\VID_0C72&`,
	}
	watcherContexts.Store(watcher.contextID, watcher)
	t.Cleanup(func() { watcherContexts.Delete(watcher.contextID) })
	return watcher
}

func TestNotifyFiltersBeforeQueue(t *testing.T) {
	watcher := newUnregisteredTestWatcher(t)
	unrelated := encodeInstanceEventData(t, `USB\VID_1234&PID_5678\example`)
	peakIdentifier := `usb\vid_0c72&PID_000C\example`
	peak := encodeInstanceEventData(t, peakIdentifier)
	dispatch := func(action Action, buffer []byte) {
		dispatchNotification(
			0,
			watcher.contextID,
			uintptr(action),
			unsafe.Pointer(&buffer[0]),
			uintptr(len(buffer)),
		)
	}

	// Far more churn than the queue holds: unrelated removals and matching
	// non-removal transitions must never occupy a slot.
	for range 4 * watcherBuffer {
		dispatch(ActionInstanceRemoved, unrelated)
		dispatch(ActionInstanceEnumerated, peak)
		dispatch(ActionInstanceStarted, peak)
	}
	dispatch(ActionInstanceRemoved, peak)

	select {
	case event := <-watcher.Events():
		if event.InstanceID != peakIdentifier {
			t.Fatalf("delivered event = %+v", event)
		}
	default:
		t.Fatal("matching removal was not delivered after unrelated churn")
	}
	if watcher.Lost() {
		t.Fatal("unrelated churn consumed queue slots")
	}

	// Only matching removals can overrun the queue, and overrun sets Lost
	// exactly until it is read.
	for range watcherBuffer + 1 {
		dispatch(ActionInstanceRemoved, peak)
	}
	if !watcher.Lost() {
		t.Fatal("queue overrun did not set Lost")
	}
	if watcher.Lost() {
		t.Fatal("Lost did not clear once read")
	}
}

func TestDispatchNotificationRoutesByContext(t *testing.T) {
	addressed := newUnregisteredTestWatcher(t)
	bystander := newUnregisteredTestWatcher(t)

	identifier := `USB\VID_0C72&PID_000C\example`
	buffer := encodeInstanceEventData(t, identifier)
	dispatch := func(contextID uintptr) {
		dispatchNotification(
			0,
			contextID,
			uintptr(ActionInstanceRemoved),
			unsafe.Pointer(&buffer[0]),
			uintptr(len(buffer)),
		)
	}

	dispatch(addressed.contextID)
	select {
	case event := <-addressed.Events():
		if event.InstanceID != identifier {
			t.Fatalf("delivered event = %+v", event)
		}
	default:
		t.Fatal("dispatch did not deliver to the addressed watcher")
	}
	select {
	case event := <-bystander.Events():
		t.Fatalf("dispatch reached an unaddressed watcher: %+v", event)
	default:
	}

	// A zero context and an issued-but-never-registered context drop silently.
	dispatch(0)
	dispatch(nextWatcherContext.Add(1))

	watcherContexts.Delete(addressed.contextID)
	dispatch(addressed.contextID)
	select {
	case event := <-addressed.Events():
		t.Fatalf("dispatch delivered to a deregistered watcher: %+v", event)
	default:
	}
}
