# gocan

`gocan` is a Go stack for communicating with automotive CAN networks
across multiple hardware vendors.

The project is in its foundation stage. A temporary
[working specification](GOCAN_SPEC.md) guides the work while the public API
settles. Source and package documentation define current behaviour.

## Driver support

| Driver | Platform | CAN | CAN FD | Current limits |
| --- | --- | --- | --- | --- |
| `drivers/virtual` | Portable, including macOS | Yes | Yes | Development and tests only |
| SocketCAN through `drivers` | Linux | Yes | Yes | The Linux link configuration owns bit timing; physical-adapter qualification remains open |
| PCAN through `drivers` | Windows | Yes | Yes | Classical configuration supports 500 kbit/s; PCAN-Basic cannot select ISO/non-ISO framing, so the adapter's stored mode must match its peers; the classical API cannot transmit classical DLC 9–15 |
| Vector through `drivers` | Windows x64 | Yes | Yes | CAN FD uses an 80 MHz clock and ISO framing; error-state-indicator transmission is not supported |

macOS is a development target through the virtual driver. It has no physical
hardware driver.

The `drivers` package is the physical-hardware API. Discover a channel and pass
that opaque value to `Open`:

```go
channels, err := drivers.Discover()
if err != nil {
	return err
}
if len(channels) == 0 {
	return errors.New("no physical CAN channels")
}
channel := channels[0]
config := drivers.Config{ID: 1, Name: "powertrain", Bitrate: 500_000}
if channel.ExternallyConfigured() {
	// Linux owns SocketCAN link timing.
	config = drivers.Config{ID: 1, Name: "powertrain", External: true}
}
bus, err := drivers.Open(ctx, capture, channel, config)
```

PCAN and Vector CAN FD channels accept the same exact bit-timing value.
`FDTiming` does not alter the stored PCAN ISO/non-ISO framing mode:

```go
timing := drivers.FDTiming{
	ClockHz: 80_000_000,
	Nominal: drivers.BitTiming{BRP: 1, TSEG1: 119, TSEG2: 40, SJW: 1},
	Data:    drivers.BitTiming{BRP: 1, TSEG1: 29, TSEG2: 10, SJW: 1},
}
bus, err := drivers.Open(ctx, capture, channel, drivers.Config{
	ID: 1, Name: "powertrain", FDTiming: timing,
})
```

The Windows PCAN and Vector paths are qualified on physical classic and CAN FD
adapters. SocketCAN passes the software and virtual-interface suites; its
[physical-adapter qualification](https://github.com/tomrford/gocan/issues/10)
remains release work. NI-XNET is deferred until hardware is available.

## Development

The module requires Go 1.25 or newer:

```sh
go test ./...
go vet ./...
```

`gocan` is licensed under the [MIT License](LICENSE).
