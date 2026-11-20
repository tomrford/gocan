//go:build linux

package socketcan

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"testing"
	"time"

	"github.com/tomrford/gocan"
	"golang.org/x/sys/unix"
)

// TestVCanInjectedErrorPackets sends synthetic Linux error-message packets
// through vcan. It exercises socket delivery, decoding, Capture order, and
// bus-off recovery reporting; it does not make a CAN controller enter these
// states.
func TestVCanInjectedErrorPackets(t *testing.T) {
	iface := os.Getenv("GOCAN_VCAN_INTERFACE")
	if iface == "" {
		t.Skip("GOCAN_VCAN_INTERFACE is not set")
	}

	capture := gocan.NewCapture()
	bus, err := Open(context.Background(), capture, Config{ID: 1, Name: "error-events", Interface: iface, FD: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	sender := openErrorSender(t, iface)
	sendErrorPacket(t, sender, unix.CAN_ERR_CRTL|unix.CAN_ERR_CNT, [8]byte{1: unix.CAN_ERR_CRTL_RX_WARNING, 6: 12, 7: 34})
	sendErrorPacket(t, sender, unix.CAN_ERR_PROT|unix.CAN_ERR_CRTL|unix.CAN_ERR_CNT, [8]byte{1: unix.CAN_ERR_CRTL_TX_PASSIVE, 6: 128, 7: 129})
	sendErrorPacket(t, sender, unix.CAN_ERR_CRTL, [8]byte{})
	sendErrorPacket(t, sender, unix.CAN_ERR_RESTARTED, [8]byte{})
	sendErrorPacket(t, sender, unix.CAN_ERR_BUSOFF, [8]byte{})
	sendErrorPacket(t, sender, unix.CAN_ERR_RESTARTED, [8]byte{})

	events := waitBusEvents(t, bus, 7)
	requireStateEvent(t, events[0], gocan.ControllerWarning, 12, 34, true)
	requireErrorEvent(t, events[1])
	requireStateEvent(t, events[2], gocan.ControllerPassive, 128, 129, true)
	requireErrorEvent(t, events[3])
	requireStateEvent(t, events[4], gocan.ControllerActive, 0, 0, false)
	requireStateEvent(t, events[5], gocan.ControllerBusOff, 0, 0, false)
	requireStateEvent(t, events[6], gocan.ControllerActive, 0, 0, false)
	requireBusOpen(t, bus)
}

func TestVCanInjectedReceiveOverrun(t *testing.T) {
	iface := os.Getenv("GOCAN_VCAN_INTERFACE")
	if iface == "" {
		t.Skip("GOCAN_VCAN_INTERFACE is not set")
	}

	capture := gocan.NewCapture()
	bus, err := Open(context.Background(), capture, Config{ID: 1, Name: "receive-overrun", Interface: iface, FD: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	sender := openErrorSender(t, iface)
	sendErrorPacket(t, sender, unix.CAN_ERR_CRTL|unix.CAN_ERR_CNT, [8]byte{
		1: unix.CAN_ERR_CRTL_RX_OVERFLOW | unix.CAN_ERR_CRTL_RX_WARNING,
		6: 3,
		7: 96,
	})

	events := waitBusEvents(t, bus, 2)
	requireStateEvent(t, events[0], gocan.ControllerWarning, 3, 96, true)
	if events[1].Kind != gocan.EventReceiveOverrun {
		t.Fatalf("event = %+v, want receive-overrun event", events[1])
	}
	requireBusOpen(t, bus)
}

func waitBusEvents(t *testing.T, bus *Bus, count int) []gocan.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		events := bus.Capture().BusEvents(bus.ID())
		if len(events) > count {
			t.Fatalf("captured %d events, want %d: %+v", len(events), count, events)
		}
		if len(events) == count {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("captured %d events, want %d: %+v (bus error %v)", len(events), count, events, bus.Err())
		}
		time.Sleep(time.Millisecond)
	}
}

func requireBusOpen(t *testing.T, bus *Bus) {
	t.Helper()
	select {
	case <-bus.Done():
		t.Fatalf("bus stopped: %v", bus.Err())
	default:
	}
}

func openErrorSender(t testing.TB, name string) int {
	t.Helper()
	iface, err := net.InterfaceByName(name)
	if err != nil {
		t.Fatalf("find interface: %v", err)
	}
	fd, err := unix.Socket(unix.AF_CAN, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.CAN_RAW)
	if err != nil {
		t.Fatalf("open error sender: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.Bind(fd, &unix.SockaddrCAN{Ifindex: iface.Index}); err != nil {
		t.Fatalf("bind error sender: %v", err)
	}
	return fd
}

func sendErrorPacket(t *testing.T, fd int, classes uint32, data [8]byte) {
	t.Helper()
	var packet [classicalMTU]byte
	binary.NativeEndian.PutUint32(packet[:4], unix.CAN_ERR_FLAG|classes)
	packet[4] = 8
	copy(packet[8:], data[:])
	written, err := unix.Write(fd, packet[:])
	if err != nil {
		t.Fatalf("send error packet: %v", err)
	}
	if written != len(packet) {
		t.Fatalf("send error packet wrote %d of %d bytes", written, len(packet))
	}
}

func requireErrorEvent(t *testing.T, event gocan.Event) {
	t.Helper()
	if event.Kind != gocan.EventErrorFrame {
		t.Fatalf("event = %+v, want error-frame event", event)
	}
}

func requireStateEvent(
	t *testing.T,
	event gocan.Event,
	state gocan.ControllerState,
	txErrorCount uint8,
	rxErrorCount uint8,
	countsKnown bool,
) {
	t.Helper()
	if event.Kind != gocan.EventControllerState ||
		event.ControllerState != state ||
		event.TXErrorCount != txErrorCount ||
		event.RXErrorCount != rxErrorCount ||
		event.ErrorCountsKnown != countsKnown {
		t.Fatalf("event = %+v, want state %d, counters (%d, %d, known=%t)",
			event, state, txErrorCount, rxErrorCount, countsKnown)
	}
}
