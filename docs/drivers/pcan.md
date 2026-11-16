# PCAN

The PCAN classical API cannot transmit classical DLC values 9–15. PCAN-Basic
cannot select ISO or non-ISO CAN FD framing, so the adapter's stored mode must
match its peers.

PCAN `Send` returns once PCAN-Basic queues the frame. On normal `Close`, gocan
stops accepting sends and gives previously accepted frames an additional
100 ms to drain before uninitializing the channel. `Config.PCANCloseGracePeriod`
overrides this interval; zero selects the default and a negative value skips
the added wait. The interval starts at the first `Close` call. No grace period
is added if nothing was sent or the bus had already failed.

The default adds margin over roughly 90 ms of delayed traffic observed on a
PCAN-USB. This is best-effort draining: PCAN-Basic can add its own shutdown
wait, and gocan cannot confirm delivery or guarantee that no retained frames
appear on a later open. Wait for replies before closing when delivery matters.
The grace period does not change bitrate or controller mode.
