# gocan

`gocan` is a Go stack for communicating with automotive CAN networks
across multiple hardware vendors.

The project is in its foundation stage. A temporary
[working specification](GOCAN_SPEC.md) guides the work while the public API
settles. Source and package documentation define current behaviour.

## Driver support

| Driver | Use | Platform | CAN | CAN FD | Current limits |
| --- | --- | --- | --- | --- | --- |
| `virtual` | Development and tests only | Portable, including macOS | Yes | Yes | No physical hardware |
| `socketcan` | Physical hardware | Linux | Yes | Yes | Tested only with `vcan`; see the qualification note below |
| `pcan` | Physical PEAK hardware | Windows | Yes | Yes | The classical API cannot transmit classical DLC 9–15 (the FD API preserves them); CAN FD peers must agree on ISO or non-ISO framing, which follows the adapter's stored device configuration because PCAN-Basic cannot select it |
| `vector` | Physical Vector hardware | Windows x64 | Yes | Yes | CAN FD error-state-indicator transmission is not supported |

macOS is currently a development target through the virtual driver. It has no
physical-hardware driver.

The `socketcan` implementation has been tested only with Linux `vcan`
interfaces. [Physical-hardware qualification](https://github.com/tomrford/gocan/issues/10)
is still required to qualify adapter removal, native controller error frames,
transmit-queue saturation, and any refinements those results require.

Each physical driver exposes its own `Discover`, and the `drivers` package
aggregates them into one platform-wide channel inventory.

## Development

The module currently requires Go 1.25 or newer:

```sh
go test ./...
go vet ./...
```

`gocan` is licensed under the [MIT License](LICENSE).
