# `tomrford/gocan`

## Working specification

**Status:** Foundation implementation

**Module:** `github.com/tomrford/gocan`

**Licence:** MIT

**Language:** Go

**Platforms:** Portable development support, Windows hardware drivers, and
Linux hardware support through SocketCAN

This is a temporary working plan for the foundation stage. It records intended
scope and design direction while the public API settles. It is not the contract
for current behaviour and should be deleted after the first stable release.
Source and package documentation define implemented behaviour. GitHub issues
track work that does not need to remain beside the affected code.

## Purpose

`gocan` is a public Go stack for communicating with automotive CAN networks
across multiple hardware vendors.

Its intended boundary is:

> Tell `gocan` which hardware to use, provide the relevant network or diagnostic
> description files, and use it to send or receive raw frames, named messages,
> signals, transport payloads, or diagnostic operations.

Consumers may enter at any layer. Higher packages build on raw CAN without
hiding the lower packages.

## Design direction

- Keep the required hardware contract small and provider-neutral.
- Keep vendor configuration and native details inside each driver package.
- Store traffic as raw frames and decode semantic values only when requested.
- Encode named transmissions before they enter recurring or time-sensitive
  paths.
- Use explicit ownership, cancellation, shutdown, and overflow behaviour.
- Add capabilities and public API only after a real driver or consumer requires
  them.
- Keep package boundaries cohesive without splitting the project into separate
  modules.

The API remains open while materially different drivers and higher layers test
these decisions. Source code is authoritative for implemented behaviour.

## Implemented foundation

### Raw CAN

The root `gocan` package defines:

- classical CAN and CAN FD frames;
- bus identity and the common [`Bus`](bus.go) contract;
- received and transmitted frame events;
- controller-state, error-frame, and receive-overrun events;
- the caller-owned [`Capture`](capture.go), its cursors, queries, and ordered
  record writer;
- shared lifecycle and error values, including transmit backpressure and
  hardware disconnection.

One bus owns one logical CAN channel. It accepts raw transmissions and appends
accepted TX traffic and incoming RX traffic to a caller-owned capture. Several
buses may contribute to the same capture. Closing one bus does not clear or
close shared history.

Capture timestamps use host wall-clock time and append order is authoritative.
Provider-native timestamps are not retained. Capture storage stays raw and has
no automatic retention limit; the caller controls persistence and clearing.

The implemented data model is defined by [`frame.go`](frame.go),
[`event.go`](event.go), [`bus.go`](bus.go), and [`capture.go`](capture.go).

### Drivers

Implemented driver packages are:

- [`drivers/virtual`](drivers/virtual) for portable development and contract
  tests;
- [`drivers`](drivers) for physical SocketCAN, PEAK PCAN, and Vector XL
  channels.

Every driver contributes to the same capture and follows the shared lifecycle
contract. Physical channels are discovered and opened through one generic
configuration. SocketCAN uses externally configured link timing; programmable
drivers accept a classical bitrate or exact CAN FD bit timing. Vector uses ISO
CAN FD; PCAN framing follows the adapter's stored ISO/non-ISO mode. Driver
opening accepts a context without making that context own the returned bus.
The reusable behavioural checks live in
[`drivers/conformance`](drivers/conformance). Remaining release-required
hardware checks stay as precise code TODOs beside the affected drivers.
Conditional or later work belongs in GitHub issues.

NI-XNET is the next intended provider. Its implementation may refine the common
contract if its session model provides evidence that the existing shape is not
general enough.

### ASC traces

[`asc`](asc) writes retained frames and events in Vector ASCII trace format.
It implements the root record-writer contract, so export preserves capture
append order without blocking bus acquisition.

Other trace formats and live-writing policies will be added only when required.

### Recurring transmission

[`cyclic`](cyclic) owns software-scheduled transmission of complete raw frames.
Updates replace the complete frame before a later send; the timing path does no
signal encoding.

### DBC

[`dbc`](dbc) parses runtime-loaded DBC files into a resolved semantic model.
Named messages encode complete frames, patch existing complete frames, and
lazily decode one requested signal. Encoding requires every signal active on
the selected multiplexing path. Changing that path requires values for the
newly active signals.

### ISO-TP

[`isotp`](isotp) moves opaque payloads over normal-addressed physical CAN links.
It has no UDS or other application-protocol semantics.

A `Link` provides three operations:

- `Send` transmits one complete payload;
- `Receive` waits for and reassembles one complete payload;
- `Begin` transmits one payload and returns an `Exchange` that retains link
  ownership while the caller reads later payloads with `Next`.

`Begin` records the capture frontier immediately before transmission. This
prevents a reply from being satisfied by older traffic and lets a protocol such
as UDS read response-pending payloads before its final response. The cursor is
private to ISO-TP.

Send calls and receive calls are serialized independently. `Begin` owns both
paths until its exchange closes. A server must finish `Receive` before calling
`Send` on the same link because segmented receive can transmit Flow Control
without taking the independent send path. A link should have one logical owner
and one role at a time.

The current implementation supports classical CAN and CAN FD, single and
segmented payloads, Flow Control, block size, separation time, bounded receive
allocation, protocol timeouts, cancellation, and repeated exchange responses.

### UDS

[`uds`](uds) exchanges raw Unified Diagnostic Services requests and responses
over an `*isotp.Link`. `Do` validates the response service, handles
ResponsePending, and applies P2 and P2* timing. `Send` transmits requests that
do not expect a response.

## Planned higher layers

### Network descriptions

DBC support currently uses exact message identifiers, including its initial
J1939 support. J1939 PGN matching and AUTOSAR ARXML descriptions remain future
work to define from real inputs.

### UDS services

Typed standard services, session state, and OEM extension points will build on
the raw `uds` request and response values.

### Diagnostic descriptions

Useful subsets of ODX, CANdela CDD, and diagnostic ARXML may later describe
named requests, responses, DIDs, routines, and timing. Importers will be driven
by real files and will report unsupported semantics rather than silently
discarding them.

## Development method

Implement one minimal vertical slice, validate it, then expand it from measured
or observed requirements. Important sources, in order, are:

1. official standards and vendor headers;
2. vendor documentation and examples;
3. observed real-hardware behaviour;
4. differential behaviour from established libraries;
5. implementation source used as a cautious reference.

Existing Python CAN, ISO-TP, UDS, and description libraries are behavioural
oracles rather than API templates. Use code TODOs only for known work required
before the first stable release. Track conditional and later work in GitHub
issues. Keep station-specific selectors, topology, versions, and raw
qualification evidence outside the public repository.

Tests should be curated and lifecycle-focused. Use the shared driver
conformance suite, cross-provider loopback, fault injection, race detection,
parser fuzzing, and throughput measurements where they provide useful evidence.

## Licensing

The project uses the MIT licence. Dependencies and generated or adapted material
must remain licence-compatible. Behaviour observed from a public implementation
does not grant permission to copy that implementation.

## End goal

A consumer should be able to select supported hardware, exchange raw CAN or CAN
FD frames, load network and diagnostic descriptions, and work with named signals
or diagnostic operations without losing access to the lower layers.
