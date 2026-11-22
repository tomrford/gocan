# Vector

Vector CAN FD uses an 80 MHz clock and ISO framing. Built-in CAN FD rate
presets use an 80% sample point in both phases; custom CAN FD bit timing is
supported through `Config.FDTiming`. Transmitting the error-state
indicator is not supported. A Vector channel that another application, such as
CANoe, has on the bus is discovered as externally configured with its current
rates; open it with `Config.External` to join without changing its timing.
gocan must join second. Keep the owner's measurement and timing stable while
flashing; discovery and the on-bus check cannot reserve that configuration.
