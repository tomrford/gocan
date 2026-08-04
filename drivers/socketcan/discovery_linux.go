//go:build linux

package socketcan

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Interface is one Linux CAN network interface.
type Interface struct {
	Name string
	// Up reports whether the interface has the Linux IFF_UP flag.
	Up bool
	// SupportsFD reports whether the interface MTU accommodates CAN FD
	// frames, which Config.FD requires.
	SupportsFD bool
}

// Discover reports configured Linux CAN network interfaces, including
// interfaces which are down.
func Discover() ([]Interface, error) {
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETLINK, syscall.AF_UNSPEC)
	if err != nil {
		return nil, fmt.Errorf("discover SocketCAN interfaces: %w", err)
	}
	messages, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return nil, fmt.Errorf("parse SocketCAN interface list: %w", err)
	}

	var interfaces []Interface
	for index := range messages {
		message := &messages[index]
		if message.Header.Type != syscall.RTM_NEWLINK {
			continue
		}
		if len(message.Data) < syscall.SizeofIfInfomsg {
			return nil, fmt.Errorf("parse SocketCAN interface: link message has %d bytes", len(message.Data))
		}
		info := (*syscall.IfInfomsg)(unsafe.Pointer(&message.Data[0]))
		if info.Type != unix.ARPHRD_CAN {
			continue
		}

		attributes, err := syscall.ParseNetlinkRouteAttr(message)
		if err != nil {
			return nil, fmt.Errorf("parse SocketCAN interface attributes: %w", err)
		}
		name := ""
		mtu := uint32(0)
		for _, attribute := range attributes {
			switch attribute.Attr.Type {
			case syscall.IFLA_IFNAME:
				name = unix.ByteSliceToString(attribute.Value)
			case syscall.IFLA_MTU:
				if len(attribute.Value) >= 4 {
					mtu = binary.NativeEndian.Uint32(attribute.Value)
				}
			}
		}
		if name == "" {
			return nil, fmt.Errorf("parse SocketCAN interface: link %d has no name", info.Index)
		}
		interfaces = append(interfaces, Interface{
			Name:       name,
			Up:         info.Flags&unix.IFF_UP != 0,
			SupportsFD: mtu >= fdMTU,
		})
	}
	return interfaces, nil
}
