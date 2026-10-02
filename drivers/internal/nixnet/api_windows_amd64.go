//go:build windows && amd64

package nixnet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/tomrford/gocan"
	"golang.org/x/sys/windows"
)

// Constants and ABI follow NI's nixnet-python _cconsts.py and _cfuncs.py.
const (
	propInterfaces     = 0x06110004
	propName           = 0x03130002
	propDevice         = 0x05130001
	propDeviceName     = 0x03120003
	propTransceiver    = 0x00130007
	propTerminationCap = 0x00130008
	propBaud           = 0x09100016
	propFDBaud         = 0x09100027
	propEcho           = 0x02100010
	propErrors         = 0x02100015
	propTiming         = 0x00100012
	propPendingOrder   = 0x00100020
	propTermination    = 0x00100025
	propISO            = 0x0010003e
	propQueueSize      = 0x0010000c
	stateCAN           = 0x00130010
)

type api struct {
	systemOpen, systemClose, get, getSize, set, create, start, clear, read, write, state, statusText *windows.LazyProc
}

var nativeDLL = windows.NewLazySystemDLL("nixnet.dll")

func loadAPI() (*api, error) {
	dll := nativeDLL
	if err := dll.Load(); err != nil {
		if errors.Is(err, windows.ERROR_MOD_NOT_FOUND) {
			directory, directoryErr := windows.GetSystemDirectory()
			if directoryErr == nil {
				if _, statErr := os.Stat(filepath.Join(directory, dll.Name)); os.IsNotExist(statErr) {
					return nil, fmt.Errorf("%w: %v", gocan.ErrDriverUnavailable, err)
				}
			}
		}
		return nil, fmt.Errorf("load NI-XNET: %w", err)
	}
	a := &api{systemOpen: dll.NewProc("nxSystemOpen"), systemClose: dll.NewProc("nxSystemClose"), get: dll.NewProc("nxGetProperty"), getSize: dll.NewProc("nxGetPropertySize"), set: dll.NewProc("nxSetProperty"), create: dll.NewProc("nxCreateSession"), start: dll.NewProc("nxStart"), clear: dll.NewProc("nxClear"), read: dll.NewProc("nxReadFrame"), write: dll.NewProc("nxWriteFrame"), state: dll.NewProc("nxReadState"), statusText: dll.NewProc("nxStatusToString")}
	for _, p := range []*windows.LazyProc{a.systemOpen, a.systemClose, a.get, a.getSize, a.set, a.create, a.start, a.clear, a.read, a.write, a.state, a.statusText} {
		if err := p.Find(); err != nil {
			return nil, fmt.Errorf("load NI-XNET %s: %w", p.Name, err)
		}
	}
	return a, nil
}

func (a *api) check(operation string, result uintptr) error {
	status := uint32(result)
	if status == 0 {
		return nil
	}
	var description [2048]byte
	a.statusText.Call(uintptr(status), uintptr(len(description)), uintptr(unsafe.Pointer(&description[0])))
	message := bytes.TrimRight(description[:], "\x00")
	var cause error
	switch status {
	case 0xbff63008:
		cause = gocan.ErrTransmitQueueFull
	case 0xbff6300b, 0xbff630b6:
		cause = gocan.ErrReceiveOverrun
	case 0xbff630b2, 0xbff630b9, 0xbff63029:
		cause = gocan.ErrHardwareDisconnected
	}
	err := fmt.Errorf("%s: NI-XNET %#x: %s", operation, status, message)
	if cause != nil {
		return fmt.Errorf("%w: %v", cause, err)
	}
	// Nonzero warning statuses are also rejected: setup must not silently alter timing.
	return err
}

func (a *api) property(ref uint32, property uintptr) ([]byte, error) {
	var size uint32
	result, _, _ := a.getSize.Call(uintptr(ref), property, uintptr(unsafe.Pointer(&size)))
	if err := a.check("size NI-XNET property", result); err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	if size > 1<<20 {
		return nil, fmt.Errorf("NI-XNET property size %d exceeds limit", size)
	}
	data := make([]byte, size)
	result, _, _ = a.get.Call(uintptr(ref), property, uintptr(size), uintptr(unsafe.Pointer(&data[0])))
	return data, a.check("get NI-XNET property", result)
}

func (a *api) set32(ref uint32, property uintptr, value uint32) error {
	result, _, _ := a.set.Call(uintptr(ref), property, 4, uintptr(unsafe.Pointer(&value)))
	return a.check("set NI-XNET property", result)
}
func (a *api) set64(ref uint32, property uintptr, value uint64) error {
	result, _, _ := a.set.Call(uintptr(ref), property, 8, uintptr(unsafe.Pointer(&value)))
	return a.check("set NI-XNET timing", result)
}

type ChannelInfo struct {
	Interface           string
	Name                string
	SupportsFD          bool
	SupportsTermination bool
}

func Discover() (channels []ChannelInfo, err error) {
	a, err := loadAPI()
	if err != nil {
		return nil, err
	}
	var system uint32
	result, _, _ := a.systemOpen.Call(uintptr(unsafe.Pointer(&system)))
	if err := a.check("open NI-XNET system", result); err != nil {
		return nil, err
	}
	defer func() {
		result, _, _ := a.systemClose.Call(uintptr(system))
		err = errors.Join(err, a.check("close NI-XNET system", result))
		if err != nil {
			channels = nil
		}
	}()
	refs, err := a.property(system, propInterfaces)
	if err != nil {
		return nil, err
	}
	if len(refs)%4 != 0 {
		return nil, errors.New("NI-XNET interface list has invalid size")
	}
	for offset := 0; offset < len(refs); offset += 4 {
		ref := binary.LittleEndian.Uint32(refs[offset:])
		name, e := a.property(ref, propName)
		if e != nil {
			return nil, e
		}
		tcvr, e := a.property(ref, propTransceiver)
		if e != nil {
			return nil, e
		}
		term, e := a.property(ref, propTerminationCap)
		if e != nil {
			return nil, e
		}
		device, e := a.property(ref, propDevice)
		if e != nil {
			return nil, e
		}
		if len(tcvr) != 4 || len(term) != 4 || len(device) != 4 {
			return nil, errors.New("NI-XNET interface property has invalid size")
		}
		model, e := a.property(binary.LittleEndian.Uint32(device), propDeviceName)
		if e != nil {
			return nil, e
		}
		intf := string(bytes.TrimRight(name, "\x00"))
		channels = append(channels, ChannelInfo{Interface: intf, Name: fmt.Sprintf("%s %s", bytes.TrimRight(model, "\x00"), intf), SupportsFD: binary.LittleEndian.Uint32(tcvr) == 0, SupportsTermination: binary.LittleEndian.Uint32(term) == 1 && binary.LittleEndian.Uint32(tcvr) == 0})
	}
	return channels, nil
}

func (a *api) writeError(result uintptr) error {
	err := a.check("write NI-XNET frame", result)
	// The stream API may report timeout instead of overflow for a full queue.
	// A zero-timeout, single-frame write has no partial-acceptance ambiguity.
	if uint32(result) == 0xbff6300a {
		return fmt.Errorf("%w: %v", gocan.ErrTransmitQueueFull, err)
	}
	return err
}
