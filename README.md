# gocan

`gocan` is a Go module for communicating with automotive CAN and CAN FD
networks. It provides one stack from hardware access and raw capture through
ISO-TP and UDS, with semantic codecs for DBC and CANdela diagnostic data.

The public API may change before version 1.

Callers send frames on a [`Bus`](https://pkg.go.dev/github.com/tomrford/gocan#Bus)
and observe traffic from that bus's [`Capture`](https://pkg.go.dev/github.com/tomrford/gocan#Capture).
Drivers append every accepted transmission and every received frame to the
capture. ISO-TP, UDS, J1939, and XCP read that same history. Package docs are
the API reference; the sections below are the entry points.

## Install

The module requires Go 1.25 or newer.

```sh
go get github.com/tomrford/gocan
```

## Quick start

[`drivers/virtual`](https://pkg.go.dev/github.com/tomrford/gocan/drivers/virtual)
exchanges frames in-process. Create one capture and
pass it to every bus. Buses opened on the same `Network` exchange frames.
`NewFrame` validates the identifier, flags, and payload length. A classical
frame carries at most 8 bytes. A CAN FD frame uses `FrameFD` and a legal FD
length (0–8, then 12, 16, 20, 24, 32, 48, or 64). `NewRemoteFrame` builds a
classical remote frame.

`Send` returns when the bus has accepted the frame. The capture records that
acceptance, then the peer's reception. `Capture.Next` waits for one frame key
after a cursor. `Capture.End` is the cursor to pass when only later frames
matter. `FramesSince` with a zero cursor is a snapshot of every retained frame.

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

	classic, err := gocan.NewFrame(0x123, []byte{1, 2, 3, 4}, 0)
	if err != nil {
		log.Fatal(err)
	}
	fd, err := gocan.NewFrame(
		0x456,
		[]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		gocan.FrameFD|gocan.FrameBitRateSwitch,
	)
	if err != nil {
		log.Fatal(err)
	}

	cursor := capture.End()
	if err := sender.Send(ctx, classic); err != nil {
		log.Fatal(err)
	}
	if err := sender.Send(ctx, fd); err != nil {
		log.Fatal(err)
	}

	wait, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	received, cursor, err := capture.Next(wait, gocan.FrameKey{
		Bus:       receiver.ID(),
		ID:        classic.ID,
		Direction: gocan.DirectionReceive,
	}, cursor)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("classic %x\n", received.Frame.Data[:received.Frame.DataLength()])

	if _, _, err := capture.Next(wait, gocan.FrameKey{
		Bus:       receiver.ID(),
		ID:        fd.ID,
		Direction: gocan.DirectionReceive,
	}, cursor); err != nil {
		log.Fatal(err)
	}

	// Each send is stored twice: once as the sender's transmission and once as
	// the peer's reception.
	frames, _, err := capture.FramesSince(gocan.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	if len(frames) != 4 {
		log.Fatalf("captured %d frames", len(frames))
	}

	if err := sender.Close(); err != nil {
		log.Fatal(err)
	}
	<-sender.Done()
	if err := sender.Err(); err != nil {
		log.Fatal(err)
	}
}
```

`Close` is idempotent and waits until the bus has stopped. `Done` is closed
when the bus stops for any reason. `Err` is nil after a normal `Close` and is
the terminal error when a background failure stopped the bus. A later `Send`
returns that error, or `ErrBusClosed` after a normal close. Select on `Done`
while the bus is open to observe completion without calling `Close`.

`gocan.Send` retries `ErrTransmitQueueFull` for a positive `retryTimeout`.
A zero timeout makes one attempt. Other send errors return immediately.
`ErrReceiveOverrun` means the driver stopped the bus because its receive queue
filled. `ErrBusOff`, `ErrHardwareDisconnected`, and `ErrDriverUnavailable` are
the other terminal bus errors.

Records stay until `Clear` or `Prune`. A cursor the capture can no longer
place returns `ErrCursorOutOfRange`. `Latest` returns the newest frame for a
key without waiting. `Series` and `SeriesSince` return one key's history.

## Open a physical channel

[`drivers.Discover`](https://pkg.go.dev/github.com/tomrford/gocan/drivers#Discover)
lists channels from the installed stacks. Pass a discovered `Channel` to
[`Open`](https://pkg.go.dev/github.com/tomrford/gocan/drivers#Open) unchanged.
Set exactly one of `Bitrate` (and, for a preset CAN FD pair, `DataBitrate`),
`FDTiming`, or `External`. SocketCAN is externally timed: Linux owns the
bitrate, so `External` is required. `FDRatePresets` lists gocan-provided
nominal/data pairs for programmable FD channels. The
[driver tables](#driver-support) below are the hardware summary.

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers"
)

func main() {
	channels, err := drivers.Discover()
	if err != nil {
		log.Fatal(err)
	}
	if len(channels) == 0 {
		fmt.Println("no physical CAN channel")
		return
	}

	channel := channels[0]
	fmt.Printf("%s %s fd=%t external=%t\n",
		channel.Driver(), channel.Name(), channel.SupportsFD(), channel.ExternallyConfigured())

	config := drivers.Config{ID: 1, Name: channel.Name()}
	switch {
	case channel.ExternallyConfigured():
		config.External = true
	default:
		presets := channel.FDRatePresets()
		if len(presets) > 0 {
			config.Bitrate = presets[0].Bitrate
			config.DataBitrate = presets[0].DataBitrate
		} else {
			config.Bitrate = 500_000
		}
	}
	if channel.SupportsTermination() {
		config.Termination = drivers.TerminationOn
	}

	capture := gocan.NewCapture()
	bus, err := drivers.Open(context.Background(), capture, channel, config)
	if err != nil {
		log.Fatal(err)
	}
	defer bus.Close()
	fmt.Println("open", bus.Name())
}
```

`Config.NIXNETConcurrentIO` applies only when opening an NI-XNET channel. See
[Driver restrictions](#driver-restrictions).

## Export a trace

[`asc.Writer`](https://pkg.go.dev/github.com/tomrford/gocan/asc) writes a Vector
ASCII trace. `WriteRecordsSince` walks the capture in append order, frames and
non-frame events together. The zero cursor exports everything still retained.
[`recorder.Start`](https://pkg.go.dev/github.com/tomrford/gocan/recorder#Start)
keeps writing records that arrive later. [`mf4.Writer`](https://pkg.go.dev/github.com/tomrford/gocan/mf4)
writes an MDF 4.10 file.

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/asc"
	"github.com/tomrford/gocan/drivers/virtual"
)

func main() {
	ctx := context.Background()
	capture := gocan.NewCapture()
	var network virtual.Network
	bus, err := network.Open(ctx, capture, virtual.Config{ID: 1, Name: "bus"})
	if err != nil {
		log.Fatal(err)
	}
	defer bus.Close()

	frame, err := gocan.NewFrame(0x100, []byte{0x11, 0x22}, 0)
	if err != nil {
		log.Fatal(err)
	}
	if err := bus.Send(ctx, frame); err != nil {
		log.Fatal(err)
	}

	file, err := os.Create("trace.asc")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	writer := asc.NewWriter(file)
	if _, err := capture.WriteRecordsSince(gocan.Cursor{}, writer); err != nil {
		log.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		log.Fatal(err)
	}
}
```

[`cyclic.Start`](https://pkg.go.dev/github.com/tomrford/gocan/cyclic#Start)
repeats one raw frame on a bus. The task reports send failures through
`Done` and `Err`.

## ISO-TP and UDS

[`isotp.New`](https://pkg.go.dev/github.com/tomrford/gocan/isotp#New) binds a
normal-addressed transmit and receive identifier to a bus. A link reads its
receive identifier from the bus capture. [`uds.New`](https://pkg.go.dev/github.com/tomrford/gocan/uds#New)
sends diagnostic requests on that link. `ReadDataByIdentifier` returns the
data record after the identifier echo. Finish or recover an exchange before
reusing the receive address: ISO-TP has no transaction identifier.

This pair of virtual endpoints completes one single-frame read. A physical bus
uses the same client calls against an ECU.

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/tomrford/gocan"
	"github.com/tomrford/gocan/drivers/virtual"
	"github.com/tomrford/gocan/isotp"
	"github.com/tomrford/gocan/uds"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	capture := gocan.NewCapture()
	var network virtual.Network
	tester, err := network.Open(ctx, capture, virtual.Config{ID: 1, Name: "tester"})
	if err != nil {
		log.Fatal(err)
	}
	defer tester.Close()
	ecu, err := network.Open(ctx, capture, virtual.Config{ID: 2, Name: "ecu"})
	if err != nil {
		log.Fatal(err)
	}
	defer ecu.Close()

	testerLink, err := isotp.New(tester, isotp.Config{TransmitID: 0x7e0, ReceiveID: 0x7e8})
	if err != nil {
		log.Fatal(err)
	}
	ecuLink, err := isotp.New(ecu, isotp.Config{TransmitID: 0x7e8, ReceiveID: 0x7e0})
	if err != nil {
		log.Fatal(err)
	}
	client, err := uds.New(testerLink, uds.Config{})
	if err != nil {
		log.Fatal(err)
	}

	errc := make(chan error, 1)
	go func() {
		request, err := ecuLink.Receive(ctx)
		if err != nil {
			errc <- err
			return
		}
		errc <- ecuLink.Send(ctx, []byte{0x62, request[1], request[2], 0x2a})
	}()

	record, err := client.ReadDataByIdentifier(ctx, 0xf190)
	if err != nil {
		log.Fatal(err)
	}
	if err := <-errc; err != nil {
		log.Fatal(err)
	}
	if len(record) != 1 || record[0] != 0x2a {
		log.Fatalf("record = %x", record)
	}
}
```

`Client.Do` exchanges a raw `Request`. Typed helpers such as
`DiagnosticSessionControl` and `TesterPresent` encode one service. The caller
keeps session, security, and workflow policy.

## Decode with a description file

### DBC

[`dbc.Parse`](https://pkg.go.dev/github.com/tomrford/gocan/dbc#Parse) and
`ParseFile` resolve a database. `Encode` builds a frame from physical signal
values. `Decode` returns one signal. Scaled signals come back as `float64`.
Messages longer than one frame use `EncodePayload` and `DecodePayload` with
[`j1939`](https://pkg.go.dev/github.com/tomrford/gocan/j1939): `Decoder`
reassembles captured transport sessions, and `Open` claims an address and
transmits.

```go
package main

import (
	"fmt"
	"log"

	"github.com/tomrford/gocan/dbc"
)

func main() {
	database, err := dbc.Parse("engine.dbc", `
BU_: ECU
BO_ 256 EngineSpeed: 8 ECU
 SG_ RPM : 0|16@1+ (0.125,0) [0|8031.875] "rpm" ECU
`)
	if err != nil {
		log.Fatal(err)
	}
	message, ok := database.MessageByName("EngineSpeed")
	if !ok {
		log.Fatal("missing EngineSpeed")
	}

	frame, err := message.Encode(dbc.Values{"RPM": 800.0})
	if err != nil {
		log.Fatal(err)
	}
	rpm, err := message.Decode(frame, "RPM")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(rpm)
	if rpm != 800.0 {
		log.Fatalf("RPM = %v", rpm)
	}
}
```

### CDD

[`cdd.Parse`](https://pkg.go.dev/github.com/tomrford/gocan/cdd#Parse) and
`ParseFile` read a CANdela document. `Select` resolves one ECU and variant.
`DIDByName` finds a read or write service, and the data-record codec encodes
and decodes the record bytes. Service execution stays with the UDS client.

```go
package main

import (
	"fmt"
	"log"

	"github.com/tomrford/gocan/cdd"
)

func main() {
	document, err := cdd.Parse("ecu.cdd", []byte(`
<CANDELA dtdvers="10.0.108"><ECUDOC>
<DATATYPES><IDENT id="value"><CVALUETYPE bl="8" bo="21" enc="uns"/></IDENT></DATATYPES>
<PROTOCOLSERVICES><PROTOCOLSERVICE id="read">
  <REQ><CONSTCOMP spec="sid" bl="8" v="34"/><STATICCOMP id="identifier" spec="id" bl="16"/></REQ>
  <POS><SIMPLEPROXYCOMP id="data" dest="data"/></POS>
</PROTOCOLSERVICE></PROTOCOLSERVICES>
<DCLTMPLS><DCLTMPL id="class">
  <DCLSRVTMPL id="service" tmplref="read"/>
  <SHSTATIC id="id"><STATICCOMPREF idref="identifier"/></SHSTATIC>
  <SHPROXY id="proxy" dest="data"><PROXYCOMPREF idref="data"/></SHPROXY>
</DCLTMPL></DCLTMPLS>
<ECU><VAR><DIAGCLASS tmplref="class"><DIAGINST tmplref="class"><QUAL>Measurement</QUAL>
  <SERVICE tmplref="service"/>
  <STATICVALUE shstaticref="id" v="1"/>
  <SIMPLECOMPCONT shproxyref="proxy"><DATAOBJ dtref="value"><QUAL>Value</QUAL></DATAOBJ></SIMPLECOMPCONT>
</DIAGINST></DIAGCLASS></VAR></ECU>
</ECUDOC></CANDELA>`))
	if err != nil {
		log.Fatal(err)
	}
	catalog, err := document.Select(cdd.Selection{})
	if err != nil {
		log.Fatal(err)
	}
	did, ok := catalog.DIDByName("Measurement")
	if !ok || len(did.Read) != 1 || did.Read[0].PositiveResponse == nil {
		log.Fatal("missing Measurement DID")
	}
	record := did.Read[0].PositiveResponse.Record
	if err := record.CodecError(); err != nil {
		log.Fatal(err)
	}

	payload, err := record.Encode(cdd.Values{"Value": 42})
	if err != nil {
		log.Fatal(err)
	}
	values, err := record.Decode(payload)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(values["Value"])
	if values["Value"] != uint64(42) {
		log.Fatalf("Value = %#v", values["Value"])
	}
}
```

[`xcp.New`](https://pkg.go.dev/github.com/tomrford/gocan/xcp#New) runs one
sequential XCP-on-CAN or CAN FD session. After `Connect`, `Read` addresses
slave memory in bytes.

## Packages

| Package | Purpose |
| --- | --- |
| [`gocan`](https://pkg.go.dev/github.com/tomrford/gocan) | Raw CAN and CAN FD frames, the common bus interface, and concurrent multi-bus capture |
| [`drivers`](https://pkg.go.dev/github.com/tomrford/gocan/drivers) | Discovery and opening of physical CAN channels |
| [`drivers/virtual`](https://pkg.go.dev/github.com/tomrford/gocan/drivers/virtual) | In-process CAN networks for development and tests |
| [`cyclic`](https://pkg.go.dev/github.com/tomrford/gocan/cyclic) | Recurring raw frame transmission |
| [`asc`](https://pkg.go.dev/github.com/tomrford/gocan/asc) | Vector ASCII trace writing |
| [`mf4`](https://pkg.go.dev/github.com/tomrford/gocan/mf4) | MDF 4.10 raw CAN/CAN FD export with DBC attachments, event markers, compression and application metadata |
| [`recorder`](https://pkg.go.dev/github.com/tomrford/gocan/recorder) | Lifecycled trace recording with safe flush checkpoints |
| [`dbc`](https://pkg.go.dev/github.com/tomrford/gocan/dbc) | DBC parsing with original source retention, and CAN frame encoding and decoding |
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

NI-XNET hardware tests require a connected, terminated pair selected through
`GOCAN_NIXNET_CHANNEL_A` and `GOCAN_NIXNET_CHANNEL_B` (for example `CAN5` and
`CAN6`). They enable each endpoint's internal terminator and exercise classic
CAN and ISO FD at 500k/2M and 500k/4M. The driver requires the installed NI-XNET
runtime, with no C compiler or Python dependency in applications.

## Development

```sh
go test ./...
go vet ./...
```

`gocan` is licensed under the [MIT License](LICENSE).
