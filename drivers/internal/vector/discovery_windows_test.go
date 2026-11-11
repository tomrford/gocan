//go:build windows && amd64

package vector

import (
	"context"
	"encoding/binary"
	"os"
	"testing"

	"github.com/tomrford/gocan"
)

func TestMapXLDriverConfig(t *testing.T) {
	var driver xlDriverConfig
	binary.LittleEndian.PutUint32(driver[xlDriverChannelCountOffset:], 5)
	fdOnBus := discoveryChannel(
		"Example FD 1\x00ignored", 9, 2, 1001,
		xlBusActiveCapabilityCAN,
		xlChannelCapabilityCANFDISO|0x20000000,
	)
	setBusParams(&fdOnBus, true, xlBusTypeCAN, 500_000, 2_000_000, xlCANOpModeCANFD)
	setXLDriverChannel(&driver, 0, fdOnBus)
	setXLDriverChannel(&driver, 1, discoveryChannel(
		"FlexRay", 10, 0, 1002,
		0x00040000,
		0,
	))
	// Classic parameters leave reserved bytes where CAN FD keeps its data rate.
	classicOnBus := discoveryChannel(
		"Virtual CAN", 3, 0, 0,
		xlBusActiveCapabilityCAN,
		0x20000000,
	)
	setBusParams(&classicOnBus, true, xlBusTypeCAN, 250_000, 0xffffffff, xlCANOpModeCAN20)
	setXLDriverChannel(&driver, 2, classicOnBus)
	// Some interfaces, such as CANcaseXL, report no operating mode.
	noMode := discoveryChannel("CANcaseXL 1", 4, 0, 1003, xlBusActiveCapabilityCAN, 0)
	setBusParams(&noMode, true, xlBusTypeCAN, 500_000, 0, 0)
	setXLDriverChannel(&driver, 3, noMode)
	// An idle channel keeps the parameters it last ran with.
	idle := discoveryChannel("Example FD 2", 10, 3, 1001, xlBusActiveCapabilityCAN, xlChannelCapabilityCANFDISO)
	setBusParams(&idle, false, xlBusTypeCAN, 125_000, 1_000_000, xlCANOpModeCANFD)
	setXLDriverChannel(&driver, 4, idle)

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
			OnBus:           true,
			Bitrate:         500_000,
			DataBitrate:     2_000_000,
		},
		{
			ChannelIndex:    3,
			Name:            "Virtual CAN",
			HardwareChannel: 0,
			SupportsFD:      false,
			OnBus:           true,
			Bitrate:         250_000,
		},
		{
			ChannelIndex: 4,
			Name:         "CANcaseXL 1",
			SerialNumber: 1003,
			OnBus:        true,
			Bitrate:      500_000,
		},
		{
			ChannelIndex:    10,
			Name:            "Example FD 2",
			SerialNumber:    1001,
			HardwareChannel: 3,
			SupportsFD:      true,
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

func setBusParams(channel *xlChannelConfig, onBus bool, busType, bitrate, dataBitrate uint32, mode uint8) {
	if onBus {
		channel[xlChannelIsOnBusOffset] = 1
	}
	binary.LittleEndian.PutUint32(channel[xlChannelBusTypeOffset:], busType)
	binary.LittleEndian.PutUint32(channel[xlChannelBitrateOffset:], bitrate)
	binary.LittleEndian.PutUint32(channel[xlChannelDataBitrateOffset:], dataBitrate)
	channel[xlChannelCANOpModeOffset] = mode
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
		if channel.SerialNumber == 0 {
			t.Errorf("Vector channel %d has serial number 0", want)
		}
		if os.Getenv("GOCAN_VECTOR_FD_DATA_BITRATE") != "" && !channel.SupportsFD {
			t.Errorf("Vector channel %d does not report CAN FD capability", want)
		}
		return
	}
	t.Errorf("Vector channel %d is absent from discovery result %+v", want, channels)
}
