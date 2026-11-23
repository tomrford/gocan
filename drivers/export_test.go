package drivers

func PCANTestChannel(fd bool) Channel {
	return Channel{driver: &pcanDriver, name: "PCAN-USB (1)", native: 0x51, supportsFD: fd}
}

func VectorTestChannel(fd, external bool) Channel {
	return Channel{driver: &vectorDriver, name: "VN1640A Channel 1", native: 2, supportsFD: fd, external: external}
}

func NIXNETTestChannel(fd, termination bool) Channel {
	return Channel{driver: &nixnetDriver, name: "PCI-8512 CAN1", nativeName: "CAN1", supportsFD: fd, supportsTermination: termination}
}

func ZLGTestChannel(fd bool) Channel {
	deviceType := uint32(4) // USBCAN-II
	if fd {
		deviceType = 41 // USBCANFD-200U
	}
	ch := zlgChannel("ZLG (0:1)", deviceType, 0, 1, fd)
	ch.driver = &zlgDriver
	return ch
}

func SocketCANTestChannel(fd bool) Channel {
	return Channel{driver: &socketcanDriver, name: "can0", nativeName: "can0", supportsFD: fd, external: true}
}
