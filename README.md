# gocan

`gocan` is a Go module for communicating with automotive CAN and CAN FD
networks. It provides one stack from hardware access and raw capture through
ISO-TP and UDS, with semantic codecs for DBC and CANdela diagnostic data.

## Install

The module requires Go 1.25 or newer.

```sh
go get github.com/tomrford/gocan
```

## Quick start

Two buses on one [virtual network](https://pkg.go.dev/github.com/tomrford/gocan/drivers/virtual)
share a capture and exchange frames in-process.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
)

func main() {
	ctx := context.Background()
	capture := gocan.NewCapture()
	var network virtual.Network

	sender, err := network.Open(ctx, capture, virtual.Config{ID: 1, Name: "sender"})
	if err != nil {
		log.Fatal(err)
	}
	defer sender.Close()
	receiver, err := network.Open(ctx, capture, virtual.Config{ID: 2, Name: "receiver"})
	if err != nil {
		log.Fatal(err)
	}
	defer receiver.Close()

	frame, err := gocan.NewFrame(0x123, []byte{1, 2, 3, 4}, 0)
	if err != nil {
		log.Fatal(err)
	}
	cursor := capture.End()
	if err := sender.Send(ctx, frame); err != nil {
		log.Fatal(err)
	}

	wait, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	received, _, err := capture.Next(wait, gocan.FrameKey{
		Bus:       receiver.ID(),
		ID:        frame.ID,
		Direction: gocan.DirectionReceive,
	}, cursor)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%x\n", received.Frame.Data[:received.Frame.DataLength()])
}
```

## Packages

| Package | Purpose |
| --- | --- |
| [`gocan`](https://pkg.go.dev/github.com/tomrford/gocan) | Raw CAN and CAN FD frames, the common bus interface, and concurrent multi-bus capture |
| [`drivers`](https://pkg.go.dev/github.com/tomrford/gocan/drivers) | Discovery and opening of physical CAN channels |
| [`drivers/virtual`](https://pkg.go.dev/github.com/tomrford/gocan/drivers/virtual) | In-process CAN networks for development and tests |
| [`drivers/conformance`](https://pkg.go.dev/github.com/tomrford/gocan/drivers/conformance) | Driver-independent test suite for `gocan.Bus` implementations |
| [`cyclic`](https://pkg.go.dev/github.com/tomrford/gocan/cyclic) | Recurring raw frame transmission |
| [`asc`](https://pkg.go.dev/github.com/tomrford/gocan/asc) | Vector ASCII trace writing |
| [`mf4`](https://pkg.go.dev/github.com/tomrford/gocan/mf4) | MDF 4.10 raw CAN/CAN FD export with DBC attachments, event markers, compression and application metadata |
| [`recorder`](https://pkg.go.dev/github.com/tomrford/gocan/recorder) | Lifecycled trace recording with safe flush checkpoints |
| [`dbc`](https://pkg.go.dev/github.com/tomrford/gocan/dbc) | DBC parsing with original source retention, DBC writing, and CAN frame encoding and decoding |
| [`j1939`](https://pkg.go.dev/github.com/tomrford/gocan/j1939) | J1939 identifiers, passive decoding, active address claiming and classical BAM/RTS-CTS transport |
| [`isotp`](https://pkg.go.dev/github.com/tomrford/gocan/isotp) | ISO-TP payload transport over classical CAN and CAN FD |
| [`uds`](https://pkg.go.dev/github.com/tomrford/gocan/uds) | Raw exchanges and typed Unified Diagnostic Services operations |
| [`xcp`](https://pkg.go.dev/github.com/tomrford/gocan/xcp) | Sequential XCP-on-CAN/CAN FD sessions and raw byte-addressed memory reads |
| [`cdd`](https://pkg.go.dev/github.com/tomrford/gocan/cdd) | CANdela ECU/variant selection, diagnostic service catalogs and metadata, and data-record codecs |

## Driver support

| Driver | Platform | CAN | CAN FD | Qualification |
| --- | --- | --- | --- | --- |
| Virtual | Portable, including macOS | Yes | Yes | Development and tests |
| SocketCAN | Linux | Yes | Yes | Software and virtual interfaces |
| PCAN | Windows | Yes | Yes | Physical adapters |
| Vector | Windows x64 | Yes | Yes | Physical adapters |
| NI-XNET | Windows x64 | Yes | Yes | PXI-8512 CAN5/CAN6, ISO FD at 500k/2M and 500k/4M |
| ZLG | Windows x64 | Yes | USBCANFD | USBCAN-I against a PCAN-USB peer; USBCANFD unqualified |

Physical channels are discovered and opened through `drivers`. SocketCAN uses
link timing configured by Linux. PCAN, Vector and NI-XNET accept shared exact
CAN FD bit timing through the same public configuration; ZLG accepts only its
own rate presets.

## Driver restrictions

SocketCAN carries classical DLC values 9–15 only on interfaces configured with
`cc-len8-dlc on` (vcan passes them through). Without it, the controller sends
those frames as DLC 8 and received frames report DLC 8.

The PCAN classical API cannot transmit classical DLC values 9–15. PCAN-Basic
cannot select ISO or non-ISO CAN FD framing, so the adapter's stored mode must
match its peers.

Vector CAN FD uses an 80 MHz clock and ISO framing. Transmitting the error-state
indicator is not supported. A Vector channel that another application, such as
CANoe, has on the bus is discovered as externally configured with its current
rates; open it with `Config.External` to join without changing its timing.
gocan must join second. Keep the owner's measurement and timing stable while
flashing; discovery and the on-bus check cannot reserve that configuration.

NI-XNET uses fixed high-speed transceivers and ISO FD framing. It cannot
preserve classical DLC values 9–15 or report received error-state indicators;
transmit ESI requests are rejected. Exact timing must use multiples of 25 ns.
Above 1 Mbit/s in the data phase, transmitter delay compensation is enabled
with its offset derived from the data sample point. Switchable termination is
available through `Config.Termination` on channels whose `SupportsTermination`
method returns true; the default leaves NI's default setting.

NI-XNET serializes native reads and capture appends with transmission by default.
Set `drivers.Config.NIXNETConcurrentIO` to `true` when opening a bus to let
native reads run concurrently with sends and reduce send delays. Capture batches
remain serialized with sends, timestamps remain in host capture order, and Send
still returns when NI accepts the frame into its transmit queue.

In concurrent mode, an old reply fetched before a new request can be captured
after that request. Use it for fully awaited exchanges without stale or
unsolicited replies on the same receive address. After a timeout, cancellation,
or send-only request, finish or recover the prior exchange before reusing that
address. Serialized mode avoids this extra window, but neither mode can identify
old replies still buffered in the driver or arriving late. ISO-TP cursors discard
previously captured traffic; they do not flush the native receive queue.

ZLG supports the USBCAN-I/II family (USB ID 0471:1200) and the
USBCANFD-100U, 200U and MINI (USB IDs 3068:0009 and 04CC:1240) through ZLG's
`zlgcan.dll`. ZLG installs no system-wide 64-bit library, so Windows builds of
gocan embed ZLG's libraries
([`drivers/internal/zlg/bundle`](drivers/internal/zlg/bundle/README.md), about
4 MB, not covered by gocan's licence). When `Discover` first finds a ZLG
adapter, gocan unpacks them to `%LOCALAPPDATA%\gocan\zlg\<version>` and loads
them from there. `zlgcan.dll` saves each adapter's last settings in that
folder, so termination left at its default keeps the last setting. The
libraries need runtimes that gocan does not ship: every ZLG adapter needs the
Visual C++ 2008 SP1 x64 redistributable, and USBCANFD adapters also the Visual
C++ 2013 x64 redistributable. Without the 2008 runtime, ZLG reports itself
unavailable and `Discover` lists no ZLG channels. USBCAN adapters need ZLG's
`usbcan.sys` driver and USBCANFD adapters ZLG's USBCANFD driver. Only ZLG's
predefined rates are accepted, and
classical DLC values 9–15 cannot be sent. USBCANFD channels use ISO CAN FD,
cannot transmit the error-state indicator, and switch their internal
terminator through `Config.Termination`. USBCANFD-100U and MINI share the
200U's USB IDs, so `Discover` probes each USBCANFD device type and lists each
serial number once. USBCANFD support has not been run on hardware, so its
controller-state reads skip a poll instead of stopping the bus when they fail.
Each native transmit call on a USBCAN waits for its frames to leave the
adapter (about 1 ms per call), so `Send` queues up to 256 frames and a driver
goroutine passes all pending frames of one kind, classical or CAN FD, to one
call. A USBCAN-I sustains about 2,000 8-byte frames per second this way,
against about 1,000 with one frame per call. A call that fails takes the
adapter's one-second timeout; every frame it did not confirm is dropped and
recorded as an error-frame event, never resent, because `USBCAN.dll` can
under-report frames that did reach the bus. Transmitted frames are captured
when `Send` queues them. Received frames are read one per native call, because
multi-frame reads returned corrupted records under load. `Close`
transmits frames already queued unless a transmission fails or the bus stopped
with an error. Controller state is polled every 100 ms. Closing a USBCAN takes
about one second inside `zlgcan.dll`, and `Discover` opens and closes each
adapter that is not already open in this process to read its channel count.
Adapters held by another process are omitted.

ZLG hardware tests pair a ZLG channel with a PCAN reference adapter on one
terminated classical bus, selected through `GOCAN_ZLG_DEVICE` (the ZLG device
type and index, for example `4:0` for a USBCAN-I or `41:0` for a
USBCANFD-200U) and `GOCAN_PCAN_CHANNEL` (for example `0x51`). Setting
`GOCAN_ZLG_SOAK` to a duration such as `10m` repeats opening, bidirectional
sequenced traffic and closing until it elapses.

NI-XNET hardware tests require a connected, terminated pair selected through
`GOCAN_NIXNET_CHANNEL_A` and `GOCAN_NIXNET_CHANNEL_B` (for example `CAN5` and
`CAN6`). They enable each endpoint's internal terminator and exercise classic
CAN and ISO FD at 500k/2M and 500k/4M. The driver requires the installed NI-XNET
runtime, with no C compiler or Python dependency in applications.

## Development

The module requires Go 1.25 or newer:

```sh
go test -race ./...
go vet ./...
gofmt -l .
```

`gocan` is licensed under the [MIT License](LICENSE).
