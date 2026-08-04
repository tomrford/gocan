//go:build windows && amd64

package vector

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/tomrford/gocan"
)

func TestMapXLDriverConfig(t *testing.T) {
	var driver xlDriverConfig
	binary.LittleEndian.PutUint32(driver[xlDriverChannelCountOffset:], 3)
	setXLDriverChannel(&driver, 0, discoveryChannel(
		"Example FD 1\x00ignored", 9, 2, 1001,
		xlBusActiveCapabilityCAN,
		xlChannelCapabilityCANFDISO|0x20000000,
	))
	setXLDriverChannel(&driver, 1, discoveryChannel(
		"FlexRay", 10, 0, 1002,
		0x00040000,
		0,
	))
	setXLDriverChannel(&driver, 2, discoveryChannel(
		"Virtual CAN", 3, 0, 0,
		xlBusActiveCapabilityCAN,
		0x20000000,
	))

	got, err := mapXLDriverConfig(&driver)
	if err != nil {
		t.Fatalf("mapXLDriverConfig: %v", err)
	}
	want := []ChannelInfo{
		{
			ChannelIndex:    9,
			Name:            "Example FD 1",
			SerialNumber:    1001,
			HardwareChannel: 2,
			SupportsFD:      true,
		},
		{
			ChannelIndex:    3,
			Name:            "Virtual CAN",
			HardwareChannel: 0,
			SupportsFD:      false,
		},
	}
	if len(got) != len(want) {
		t.Fatalf("mapped channels = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("mapped channel %d = %+v, want %+v", index, got[index], want[index])
		}
	}
}

func TestMapXLDriverConfigRejectsImplausibleCount(t *testing.T) {
	var driver xlDriverConfig
	binary.LittleEndian.PutUint32(driver[xlDriverChannelCountOffset:], 65)
	if _, err := mapXLDriverConfig(&driver); err == nil {
		t.Fatal("mapXLDriverConfig accepted 65 channels")
	}
}

func TestDiscoverVectorHardware(t *testing.T) {
	channelIndex := vectorChannelIndex(t, "GOCAN_VECTOR_CHANNEL_INDEX")

	channels, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	assertDiscoveredVectorChannel(t, channels, channelIndex)

	bus, err := Open(context.Background(), gocan.NewCapture(), Config{
		ID:           1,
		Name:         "vector-discovery-test",
		ChannelIndex: channelIndex,
		Bitrate:      500_000,
	})
	if err != nil {
		t.Fatalf("Open Vector channel: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	channels, err = Discover()
	if err != nil {
		t.Fatalf("Discover while Vector channel is open: %v", err)
	}
	assertDiscoveredVectorChannel(t, channels, channelIndex)
	select {
	case <-bus.Done():
		t.Fatalf("Vector bus stopped during discovery: %v", bus.Err())
	default:
	}
}

func discoveryChannel(
	name string,
	channelIndex, hardwareChannel uint8,
	serialNumber, busCapabilities, channelCapabilities uint32,
) xlChannelConfig {
	var channel xlChannelConfig
	copy(channel[xlChannelNameOffset:], name)
	channel[xlChannelIndexOffset] = channelIndex
	channel[xlChannelHardwareChannelOffset] = hardwareChannel
	binary.LittleEndian.PutUint32(channel[xlChannelSerialNumberOffset:], serialNumber)
	binary.LittleEndian.PutUint32(channel[xlChannelBusCapabilitiesOffset:], busCapabilities)
	binary.LittleEndian.PutUint32(channel[xlChannelCapabilitiesOffset:], channelCapabilities)
	return channel
}

func setXLDriverChannel(driver *xlDriverConfig, index int, channel xlChannelConfig) {
	start := xlDriverChannelsOffset + index*xlChannelConfigSize
	copy(driver[start:start+xlChannelConfigSize], channel[:])
}

func assertDiscoveredVectorChannel(t *testing.T, channels []ChannelInfo, want ChannelIndex) {
	t.Helper()
	for _, channel := range channels {
		if channel.ChannelIndex != want {
			continue
		}
		if channel.Name == "" {
			t.Errorf("Vector channel %d has an empty name", want)
		}
		return
	}
	t.Errorf("Vector channel %d is absent from discovery result %+v", want, channels)
}
