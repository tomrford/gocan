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
| `mf4` | MDF 4.10 raw CAN/CAN FD export with DBC attachments, event markers, compression and application metadata |
| `recorder` | Lifecycled trace recording with safe flush checkpoints |
| `dbc` | DBC parsing with original source retention, and CAN frame encoding and decoding |
| `j1939` | J1939 identifiers, passive decoding, active address claiming and classical BAM/RTS-CTS transport |
| `isotp` | ISO-TP payload transport over classical CAN and CAN FD |
| `uds` | Raw exchanges and typed Unified Diagnostic Services operations |
| `xcp` | Sequential XCP-on-CAN/CAN FD sessions and raw byte-addressed memory reads |
| `cdd` | CANdela ECU/variant selection, diagnostic service catalogs and metadata, and data-record codecs |

## Driver support

| Driver | Platform | CAN | CAN FD | Qualification |
| --- | --- | --- | --- | --- |
| Virtual | Portable, including macOS | Yes | Yes | Development and tests |
| SocketCAN | Linux | Yes | Yes | Software and virtual interfaces |
| PCAN | Windows | Yes | Yes | Physical adapters |
| Vector | Windows x64 | Yes | Yes | Physical adapters |
| NI-XNET | Windows x64 | Yes | Yes | PXI-8512 CAN5/CAN6, ISO FD at 500k/2M and 500k/4M |

Physical channels are discovered and opened through `drivers`. SocketCAN uses
link timing configured by Linux. PCAN, Vector and NI-XNET accept shared exact CAN FD bit
timing through the same public configuration.

## Driver restrictions

The PCAN classical API cannot transmit classical DLC values 9–15. PCAN-Basic
cannot select ISO or non-ISO CAN FD framing, so the adapter's stored mode must
match its peers.

Vector CAN FD uses an 80 MHz clock and ISO framing. Transmitting the error-state
indicator is not supported.

NI-XNET uses fixed high-speed transceivers and ISO FD framing. It cannot
preserve classical DLC values 9–15 or report received error-state indicators;
transmit ESI requests are rejected. Exact timing must use multiples of 25 ns.
Above 1 Mbit/s in the data phase, transmitter delay compensation is enabled
with its offset derived from the data sample point. Switchable termination is
available through `Config.Termination` on channels whose `SupportsTermination`
method returns true; the default leaves NI's default setting.

NI-XNET hardware tests require a connected, terminated pair selected through
`GOCAN_NIXNET_CHANNEL_A` and `GOCAN_NIXNET_CHANNEL_B` (for example `CAN5` and
`CAN6`). They enable each endpoint's internal terminator and exercise classic
CAN and ISO FD at 500k/2M and 500k/4M. The driver requires the installed NI-XNET
runtime, with no C compiler or Python dependency in applications.

## Development

The module requires Go 1.25 or newer:

```sh
go test ./...
go vet ./...
```

`gocan` is licensed under the [MIT License](LICENSE).
