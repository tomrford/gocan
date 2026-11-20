# ZLG

ZLG supports USBCAN-I/II and USBCANFD-100U/200U/MINI on Windows x64.
The vendor libraries are loaded at run time and are not bundled with gocan.

## Setup

1. Install the adapter's USB driver from ZLG's
   [driver downloads](https://manual.zlg.cn/web/#/146):
   `USBCAN_AllInOne_x86_x64` for USBCAN-I/II or `USBCANFD_AllInOne_x86_x64`
   for USBCANFD-100U/200U/MINI. Unzip it and run the installer.
2. Install the Visual C++ 2013 x64 redistributable and, for USBCAN-I/II, the
   Visual C++ 2008 SP1 x64 redistributable (MFC security update), available
   from [Microsoft](https://learn.microsoft.com/en-us/cpp/windows/latest-supported-vc-redist).
3. Download the latest library from ZLG's
   [library downloads](https://manual.zlg.cn/web/#/152?page_id=5332).
   Copy the contents of `zlgcan_x64` (`zlgcan.dll` and its `kerneldlls` folder)
   beside the executable or into a folder on `PATH`. The folder must be
   writable because ZLG saves adapter settings there.

Missing libraries or runtimes cause discovery to omit ZLG channels.
Discovery opens and closes unused adapters, taking about one second per
USBCAN, and omits adapters held by another process.

## Restrictions

USBCAN-I can silently lose received frames while transmitting near full bus
load. The soak test can expose this limitation. USBCANFD has not been qualified
on hardware; tested SDKs provide error information but no controller status or
error counters for it.

Only ZLG's rate presets are accepted. Classical DLC 9–15 cannot be sent.
USBCANFD uses ISO CAN FD, cannot transmit the error-state indicator, and
supports `Config.Termination`; the default keeps ZLG's last saved setting.

`Send` captures and queues up to 256 frames. Transmissions the SDK does not
confirm produce error-frame events and are never retried, because it can
under-report frames that reached the wire. `Close` drains accepted frames unless a
transmission fails or the bus stops with an error. Native transmit failures
and adapter closing can each take about one second.

Controller and buffer overflows produce receive-overrun events; the bus keeps
receiving. When the 100 ms state poll reads bus-off, gocan waits for any
in-flight native transmit call, resets and restarts the channel as ZLG
documents, and reports the state observed on the next poll (USBCANFD restarts
without emitting a recovery state because `USBCANFD.dll` does not implement
`ReadChannelStatus`). `Send` only queues frames, so it never waits for
recovery; frames the adapter does not confirm meanwhile produce error-frame
events.

## Hardware checks

Pair a ZLG channel with a PCAN reference adapter on one terminated classical
bus, with `zlgcan_x64` on `PATH`. Set `GOCAN_ZLG_DEVICE` to the ZLG device type
and index (`4:0` for USBCAN-I or `41:0` for USBCANFD-200U) and
`GOCAN_PCAN_CHANNEL` to the PCAN handle (for example `0x51`).

`GOCAN_ZLG_SOAK=10m` repeats opening, bidirectional sequenced traffic at full
bus load and closing for ten minutes, failing on any lost frame. Retain
`go test -json ./drivers/internal/zlg` output for the cycle totals and failures.
