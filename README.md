# gocan

`gocan` is a Go module for communicating with automotive CAN and CAN FD
networks. It provides one stack from hardware access and raw capture through
ISO-TP and UDS, with semantic codecs for DBC and CANdela diagnostic data.

## Install

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

	sender, err := network.Open(ctx, capture, virtual.Config{Name: "sender"})
	if err != nil {
		log.Fatal(err)
	}
	defer sender.Close()
	receiver, err := network.Open(ctx, capture, virtual.Config{Name: "receiver"})
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
| [SocketCAN](docs/drivers/socketcan.md) | Linux | Yes | Yes | Software and virtual interfaces |
| [PCAN](docs/drivers/pcan.md) | Windows | Yes | Yes | Physical adapters |
| [Vector](docs/drivers/vector.md) | Windows x64 | Yes | Yes | Physical adapters |
| [NI-XNET](docs/drivers/nixnet.md) | Windows x64 | Yes | Yes | PXI-8512 CAN5/CAN6, ISO FD at 500k/2M and 500k/4M |
| [ZLG](docs/drivers/zlg.md) | Windows x64 | Yes | USBCANFD | USBCAN-I tested; receive loss under load; USBCANFD unqualified |

Physical channels are discovered and opened through `drivers`. SocketCAN uses
link timing configured by Linux. PCAN, Vector and NI-XNET accept shared exact
CAN FD bit timing through the same public configuration; ZLG accepts only its
own rate presets.

## Development

```sh
go test -race ./...
go vet ./...
gofmt -l .
```

`gocan` is licensed under the [MIT License](LICENSE).
