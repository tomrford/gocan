# Vector

Vector CAN FD uses an 80 MHz clock and ISO framing. Transmitting the error-state
indicator is not supported. A Vector channel that another application, such as
CANoe, has on the bus is discovered as externally configured with its current
rates; open it with `Config.External` to join without changing its timing.
gocan must join second. Keep the owner's measurement and timing stable while
flashing; discovery and the on-bus check cannot reserve that configuration.

Receive-queue overruns produce receive-overrun events; the bus keeps receiving.
gocan keeps the bus open on bus-off and reports chip-state transitions as
controller-state events; recovery depends on the adapter or, for externally
configured channels, the owning application's channel configuration.
