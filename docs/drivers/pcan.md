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
