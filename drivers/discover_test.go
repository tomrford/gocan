package drivers

import "testing"

func TestChannelString(t *testing.T) {
	tests := []struct {
		name    string
		channel Channel
		want    string
	}{
		{"pcan", Channel{driver: driverPCAN, name: "PCAN_USBBUS1", native: 0x51}, "pcan PCAN_USBBUS1 (pcan:0x51)"},
		{"socketcan", Channel{driver: driverSocketCAN, name: "can0", nativeName: "can0"}, "socketcan can0 (socketcan:can0)"},
		{"vector", Channel{driver: driverVector, name: "CANcaseXL 1"}, "vector CANcaseXL 1 (vector:0)"},
		{"nixnet", Channel{driver: driverNIXNET, name: "CAN5", nativeName: "CAN5"}, "nixnet CAN5 (nixnet:CAN5)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.channel.String(); got != test.want {
				t.Errorf("String() = %q, want %q", got, test.want)
			}
		})
	}
}
