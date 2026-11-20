# NI-XNET

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

Input-queue overflows produce one receive-overrun event each; the bus keeps
receiving. NI-XNET stays bus-off until its interface is started again, so when
the 100 ms state poll reads bus-off, gocan restarts the sessions and reports the
recovered state. Transmissions queued meanwhile can fill the transmit queue,
which `gocan.Send` retries within its timeout.
