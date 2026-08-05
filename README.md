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
| SocketCAN through `drivers` | Linux | Yes | Yes | The Linux link configuration owns bit timing |
| PCAN through `drivers` | Windows | Yes | Yes | PCAN-Basic cannot select ISO/non-ISO framing; the adapter's stored mode must match its peers. The classical API cannot transmit classical DLC 9–15 |
| Vector through `drivers` | Windows x64 | Yes | Yes | ISO CAN FD only; error-state-indicator transmission is not supported |

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
bus, err := drivers.Open(ctx, capture, channels[0], drivers.Config{
	ID: 1, Name: "powertrain", Bitrate: 500_000,
})
```

SocketCAN channels use `External: true` because Linux owns their link timing.
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

## Development

The module requires Go 1.25 or newer:

```sh
go test ./...
go vet ./...
```

`gocan` is licensed under the [MIT License](LICENSE).
