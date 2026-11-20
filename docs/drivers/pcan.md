# PCAN

The PCAN classical API cannot transmit classical DLC values 9–15. PCAN-Basic
cannot select ISO or non-ISO CAN FD framing, so the adapter's stored mode must
match its peers.

`Send` returns once PCAN-Basic queues the frame; success does not confirm
transmission on the wire or a reply. `Close` releases the channel through
PCAN-Basic, which may wait for queued transmissions during uninitialization.
gocan adds no drain delay or explicit transmit-queue flush.

PCAN-USB firmware 2.8.1 has been observed retaining queued frames after
uninitialization and transmitting them when the channel is next opened,
including by another process. Account for this in application shutdown: stop
producers and await required protocol replies before closing. A caller-chosen
delay may help queued traffic drain, but cannot guarantee delivery or an empty
adapter queue.

Receive overruns (`PCAN_ERROR_OVERRUN` and `PCAN_ERROR_QOVERRUN`) produce
receive-overrun events; the bus keeps receiving. gocan enables PCAN-Basic's
bus-off auto-reset and reports bus-off and the state the controller returns to
as controller-state events. While bus-off, `Send` returns `gocan.ErrBusOff`.

PCAN-USB (firmware 2.8.1) has reported bus-off 0.8–2.4 s after a short burst of
acknowledgement errors, with its own error frames showing a transmit error
count of at most 135, so the report can be spurious. This happens when the
adapter is the only node acknowledging, for example while the ECU resets.
Without auto-reset it cleared itself (status 0x14, then 0x8, then 0x0) and
rejected writes for about 1 s (November 2026). Give `gocan.Send` and the
protocol packages a transmit retry timeout of at least 1.5 s. A second
acknowledging node on the bus avoided the fault in three of three flashes.
