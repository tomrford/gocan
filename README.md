# gocan

`gocan` is a Go module for communicating with automotive CAN and CAN FD
networks. It provides one stack from hardware access and raw capture through
ISO-TP and UDS, with semantic codecs for DBC and CANdela diagnostic data.

## Packages

| Package | Purpose |
| --- | --- |
| `gocan` | Raw CAN and CAN FD frames, the common bus interface, and concurrent multi-bus capture |
| `drivers` | Discovery and opening of physical CAN channels |
| `drivers/virtual` | In-process CAN networks for development and tests |
| `cyclic` | Recurring raw frame transmission |
| `asc` | Vector ASCII trace writing |
| `recorder` | Lifecycled trace recording with safe flush checkpoints |
| `dbc` | DBC parsing and semantic CAN frame encoding and decoding |
| `j1939` | J1939 identifiers and passive transport-protocol reassembly |
| `isotp` | ISO-TP payload transport over classical CAN and CAN FD |
| `uds` | Raw exchanges and typed Unified Diagnostic Services operations |
| `cdd` | CANdela data-identifier parsing and semantic UDS record encoding and decoding |

## Driver support

| Driver | Platform | CAN | CAN FD | Qualification |
| --- | --- | --- | --- | --- |
| Virtual | Portable, including macOS | Yes | Yes | Development and tests |
| SocketCAN | Linux | Yes | Yes | Software and virtual interfaces |
| PCAN | Windows | Yes | Yes | Physical adapters |
| Vector | Windows x64 | Yes | Yes | Physical adapters |

Physical channels are discovered and opened through `drivers`. SocketCAN uses
link timing configured by Linux. PCAN and Vector accept shared exact CAN FD bit
timing through the same public configuration.

## Driver restrictions

The PCAN classical API cannot transmit classical DLC values 9–15. PCAN-Basic
cannot select ISO or non-ISO CAN FD framing, so the adapter's stored mode must
match its peers.

Vector CAN FD uses an 80 MHz clock and ISO framing. Transmitting the error-state
indicator is not supported.

## Development

The module requires Go 1.25 or newer:

```sh
go test ./...
go vet ./...
```

`gocan` is licensed under the [MIT License](LICENSE).
