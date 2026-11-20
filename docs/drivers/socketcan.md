# SocketCAN

SocketCAN carries classical DLC values 9–15 only on interfaces configured with
`cc-len8-dlc on` (vcan passes them through). Without it, the controller sends
those frames as DLC 8 and received frames report DLC 8.

Socket and controller receive overruns produce receive-overrun events; the bus
keeps receiving. Bus-off and the kernel's restart are reported as
controller-state events and the bus stays open. The kernel restarts a bus-off
controller only when the interface has `restart-ms` set (for example
`ip link set can0 type can restart-ms 100`); otherwise it stays bus-off until
an operator restarts it.
