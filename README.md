# Epson Stylus CX4300 scanner on Linux

Getting the scanner half of an Epson Stylus CX4300 all-in-one (`04b8:083f`)
working on Linux, after Epson's own `epkowa` driver turned out to be unusable
with it.

## Status

* Scanner protocol fully reverse engineered — see [PROTOCOL.md](PROTOCOL.md).
* `tools/escan.py` talks to the scanner directly and completes the whole
  command sequence, reading a full-size image off the device.
* **Working.** `tools/escan.py` produces a correct full-bed colour scan in
  ~25 s (1275x1755 at 150 dpi), matching what the Windows driver produces.

## Why not just use epkowa?

Epson's `epkowa` backend and its `libesint7E` plugin are the officially correct
driver for this model, and they do install and load. They still cannot scan:
`epkowa` opens by probing with ESC/I (`1b 66`), which this device does not
implement, and those bytes are an invalid SCSI CDB that leaves the scanner
refusing every later command until it is power cycled.

[FINDINGS.md](FINDINGS.md) records the full diagnosis and everything ruled out.

## Hardware requirements

Two constraints that are easy to miss:

* **No USB hub.** Attach the scanner directly to a root-hub port. Behind a hub
  the identify step fails and the scan area comes back as `0..0mm`. Internal
  chipset hubs (Intel rate-matching hubs, the internal hub on many USB 3
  add-in cards) count as hubs.
* **Nothing else may talk to it first.** One `scanimage`/`epkowa` run poisons
  the device for the rest of the power session.

If the scanner stops responding — or ignores its own power button — unplug it
from mains for ~30 s. Re-plugging USB does not clear a firmware lockup.

## Usage

```sh
# nothing else must have touched the scanner since it was powered on
sudo python3 tools/escan.py
```

Writes `/tmp/cx4300.pnm` (P6 PNM, 1275x1755 at 150 dpi). Requires
`libusb-1.0` and root, or udev rules granting access to the device.

Two things the driver has to get right, both easy to miss: the image arrives as
three colour planes per line rather than interleaved pixels, and each plane is
padded to 1280 pixels. See [PROTOCOL.md](PROTOCOL.md).

## Running it from WSL2

Useful on a machine that also has the Windows driver: `usbipd-win` passes the
device into WSL, and it arrives on a virtual root hub, which satisfies the
no-hub constraint.

```powershell
usbipd list                              # find the busid
usbipd bind --force --busid <busid>      # --force if USBPcap is installed
usbipd attach --wsl --busid <busid>
```

Then in WSL, `lsusb` should show `04b8:083f` directly under a root hub.

## Re-capturing the Windows traffic

`usbipd` submits its URBs on the Windows side, so a single USBPcap session can
record both a working Windows scan and a failing Linux one on the same port —
which is how the protocol was worked out.

* USBPcap only hooks root hubs when they start, so **reboot** after installing
  it or only one `\\.\USBPcapN` filter will exist.
* Copy `USBPcapCMD.exe` into `C:\Program Files\Wireshark\extcap\` so
  `tshark -D` lists the filters.
* Capture with `-A`; do **not** pass `-s` if you need full payloads (that is
  what truncated the gamma table).
* Scan headlessly with WIA from PowerShell — the Epson Scan GUI opens
  off-screen on Windows 11 and is unusable. If WIA reports the device busy,
  disable and re-enable the scanner's `MI_00` PnP node.
