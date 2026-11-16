# PCAN

The PCAN classical API cannot transmit classical DLC values 9–15. PCAN-Basic
cannot select ISO or non-ISO CAN FD framing, so the adapter's stored mode must
match its peers.

PCAN `Send` returns once PCAN-Basic queues the frame. `Close` sends only what
the adapter transmits while PCAN-Basic releases the channel, about 90 ms of
traffic on a PCAN-USB, and discards the rest so that no queued frame reaches
the bus later. Wait for replies before `Close` when the last frames matter.
`Close` takes about 300 ms.
