# SocketCAN

SocketCAN carries classical DLC values 9–15 only on interfaces configured with
`cc-len8-dlc on` (vcan passes them through). Without it, the controller sends
those frames as DLC 8 and received frames report DLC 8.

While bus-off, `Send` returns `gocan.ErrBusOff`. The kernel restarts a bus-off
controller only when `restart-ms` was configured while the interface was down
(for example `ip link set can0 type can restart-ms 100` before `up`); otherwise
it stays bus-off until an operator restarts it.
