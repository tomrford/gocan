//go:build windows

package pcan

import (
	"os"
	"strconv"
	"testing"
)

func TestMapPCANChannelInformation(t *testing.T) {
	tests := []struct {
		name   string
		native pcanChannelInformation
		want   ChannelInfo
	}{
		{
			name: "available FD channel",
			native: pcanChannelInformation{
				channelHandle:    ChannelUSB1,
				controllerNumber: 2,
				deviceFeatures:   pcanFeatureFD | 0x80,
				deviceName:       deviceName("PCAN-USB FD", 0xff),
				deviceID:         42,
				channelCondition: uint32(ChannelConditionAvailable),
			},
			want: ChannelInfo{
				Channel:          ChannelUSB1,
				Name:             "PCAN-USB FD",
				DeviceID:         42,
				ControllerNumber: 2,
				SupportsFD:       true,
				Condition:        ChannelConditionAvailable,
			},
		},
		{
			name: "occupied classical channel",
			native: pcanChannelInformation{
				channelHandle:    ChannelUSB2,
				deviceName:       deviceName("PCAN-USB", 0),
				channelCondition: uint32(ChannelConditionOccupied),
			},
			want: ChannelInfo{
				Channel:   ChannelUSB2,
				Name:      "PCAN-USB",
				Condition: ChannelConditionOccupied,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mapPCANChannelInformation(test.native); got != test.want {
				t.Fatalf("mapped channel = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestChannelConditionValues(t *testing.T) {
	conditions := []ChannelCondition{
		ChannelConditionUnavailable,
		ChannelConditionAvailable,
		ChannelConditionOccupied,
		ChannelConditionPCANView,
	}
	for value, condition := range conditions {
		if condition != ChannelCondition(value) {
			t.Errorf("condition %d = %d", value, condition)
		}
	}
}

func TestDiscoverPCANHardware(t *testing.T) {
	channelAValue := os.Getenv("GOCAN_PCAN_CHANNEL_A")
	channelBValue := os.Getenv("GOCAN_PCAN_CHANNEL_B")
	if channelAValue == "" || channelBValue == "" {
		t.Skip("GOCAN_PCAN_CHANNEL_A and GOCAN_PCAN_CHANNEL_B are not set")
	}

	want := map[Channel]bool{
		parseDiscoveryChannel(t, "GOCAN_PCAN_CHANNEL_A", channelAValue): false,
		parseDiscoveryChannel(t, "GOCAN_PCAN_CHANNEL_B", channelBValue): false,
	}
	channels, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, channel := range channels {
		if _, expected := want[channel.Channel]; expected {
			want[channel.Channel] = true
			if channel.Name == "" {
				t.Errorf("channel %#x has an empty name", channel.Channel)
			}
			if channel.Condition == ChannelConditionUnavailable {
				t.Errorf("channel %#x is reported unavailable", channel.Channel)
			}
		}
	}
	for channel, found := range want {
		if !found {
			t.Errorf("channel %#x is absent from discovery result %+v", channel, channels)
		}
	}
}

func deviceName(name string, trailing byte) [33]byte {
	var buffer [33]byte
	copy(buffer[:], name)
	buffer[len(name)] = 0
	for index := len(name) + 1; index < len(buffer); index++ {
		buffer[index] = trailing
	}
	return buffer
}

func parseDiscoveryChannel(t *testing.T, name, value string) Channel {
	t.Helper()
	channel, err := strconv.ParseUint(value, 0, 16)
	if err != nil || channel == 0 {
		t.Fatalf("%s=%q is not a nonzero 16-bit numeric PCAN handle", name, value)
	}
	return Channel(channel)
}
