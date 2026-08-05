//go:build linux

package socketcan

import (
	"os"
	"testing"
)

func TestDiscoverVCanState(t *testing.T) {
	upName := os.Getenv("GOCAN_VCAN_INTERFACE")
	downName := os.Getenv("GOCAN_VCAN_DOWN_INTERFACE")
	if upName == "" || downName == "" {
		t.Skip("GOCAN_VCAN_INTERFACE and GOCAN_VCAN_DOWN_INTERFACE are not set")
	}

	interfaces, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := map[string]bool{upName: true, downName: false}
	for _, iface := range interfaces {
		up, ok := want[iface.Name]
		if !ok {
			continue
		}
		if iface.Up != up {
			t.Errorf("interface %q Up = %t, want %t", iface.Name, iface.Up, up)
		}
		delete(want, iface.Name)
	}
	for name := range want {
		t.Errorf("interface %q was not discovered", name)
	}
}
