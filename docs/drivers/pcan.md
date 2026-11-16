# PCAN

The PCAN classical API cannot transmit classical DLC values 9–15. PCAN-Basic
cannot select ISO or non-ISO CAN FD framing, so the adapter's stored mode must
match its peers.

PCAN `Send` returns once PCAN-Basic queues the frame. `Close` lets the adapter
transmit for up to 100 ms after the last `Send`, then discards frames still
queued so that none reaches the bus later. Wait for replies before `Close` when
the last frames matter.
