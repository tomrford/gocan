# SocketCAN

SocketCAN carries classical DLC values 9–15 only on interfaces configured with
`cc-len8-dlc on` (vcan passes them through). Without it, the controller sends
those frames as DLC 8 and received frames report DLC 8.
