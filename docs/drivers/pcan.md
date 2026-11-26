# PCAN

For classical CAN, PCAN accepts only its predefined rates, with fractional
rates rounded to the nearest bit per second (for example `83_333`). Built-in
CAN FD rate presets use an 80% sample point in both phases; custom CAN FD bit
timing is supported through `Config.FDTiming`. The PCAN classical
API cannot transmit classical DLC values 9–15. PCAN-Basic cannot select ISO or
non-ISO CAN FD framing, so the adapter's stored mode must match its peers.

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

gocan enables PCAN-Basic's bus-off auto-reset and suppresses native writes
while bus-off so auto-reset can finish (`Send` returns `gocan.ErrBusOff`);
direct bus-off recovery reports error-active on the first clear `CAN_GetStatus`
poll. On classic PCAN-USB (firmware 2.8.1), no status frame is emitted on
recovery from warning or passive and `CAN_GetStatus` can briefly read `0x0`
between its ~1.05 s degraded status frames (on entry, after open, or after
receive overrun). Outside bus-off recovery, an error frame with both counters
below 96 reports error-active immediately; otherwise gocan polls
`CAN_GetStatus` while degraded and infers error-active (counts unknown) once
bus-state bits stay clear for 1.3 s without degraded status or overrun.

PCAN-USB (firmware 2.8.1) can report a spurious bus-off and reject writes for
about one second either as a sole-acknowledging node after a brief burst of
acknowledgement errors (for example while an ECU resets) or as TEC drains below
96 after an error-passive episode; use a transmit retry timeout of at least
1.5 s to ride it out. Adding a second acknowledging node avoided the
acknowledgement-burst fault on the bench.
