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
| `socketcan` | Physical hardware | Linux | Yes | Yes | — |
| `pcan` | Physical PEAK hardware | Windows | Yes | Yes | The classical API rejects classical DLC 9–15; the FD API path awaits FD-capable PCAN hardware qualification |
| `vector` | Physical Vector hardware | Windows | Yes | Yes | Classical DLC 9–15 is qualified on the classic API; the FD-initialized path awaits physical-link qualification; CAN FD error-state-indicator transmission is not supported |

macOS is currently a development target through the virtual driver. It has no
physical-hardware driver.

## Development

The module currently requires Go 1.25 or newer:

```sh
go test ./...
go vet ./...
```

`gocan` is licensed under the [MIT License](LICENSE).
