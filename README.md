# Epson Stylus CX4300 scanner on Linux (and Windows)

The scanner half of the Epson Stylus CX4300 family (`04b8:083f`, shared with the
CX4400, CX5500, CX5600, DX4400 and DX4450) does not work with Epson's own Linux
driver. This repository contains the reverse-engineered protocol, a Go library
that implements it, and a small web UI to scan with.

Works on Linux (direct USB) and Windows (through WIA).

## What's here

| Path | What it is |
|---|---|
| `cx4300/` | the protocol as an importable Go library |
| `cmd/escan/` | web UI: preview, crop, scan |
| `install.sh` / `install.ps1` | installers |
| [PROTOCOL.md](PROTOCOL.md) | the wire protocol, in detail |
| [FINDINGS.md](FINDINGS.md) | how it was worked out, and everything ruled out |
| `tools/escan.py` | the original Python proof of concept, kept as reference |

## Install

Linux:

```sh
sudo ./install.sh
escan                      # then open http://127.0.0.1:8080/
```

The installer builds the binary, adds a udev rule so the `scanner` group can use
the device without root, and **disables the `epkowa` SANE backend** — see the
warning below. The finished binary has no runtime dependencies: no libusb, no
Python, no SANE.

Windows:

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1
escan
```

## Two things this scanner insists on

Both cost a lot of debugging time, and neither is a software bug.

**Never let SANE touch it.** `epkowa` opens by probing with ESC/I (`1b 66`).
This device does not implement ESC/I, and that probe is an invalid SCSI command
which latches the scanner into refusing *everything* until mains power is
removed. A single `scanimage -L` is enough. Re-plugging USB does not clear it.
`install.sh` disables the backend for you; the library reports this state as
`ErrLatched`.

**Plug it straight into a root-hub port.** Behind any USB hub its identify step
fails and the scan area reads back as zero. Internal hubs count — Intel
rate-matching hubs and the internal hub on many USB 3 add-in cards included.

If it stops responding, or ignores its own power button, unplug it from mains
for ~30 seconds.

## Using the library

```go
import "github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"

sc, err := cx4300.Open()          // usbfs on Linux, WIA on Windows
if err != nil {
    log.Fatal(err)
}
defer sc.Close()

info, _ := sc.Identify()
log.Println(info.Model, info.Firmware)

// Areas are in 1/600 inch, independent of resolution.
img, err := sc.Scan(cx4300.Params{
    DPI:  150,
    Area: cx4300.Area{X: 0, Y: 0, W: 2920, H: 2960},
})
```

`cx4300.FullBed()` gives the whole platen. Supported resolutions are 75, 150,
300 and 600 dpi; `Params.Validate` rejects anything else rather than letting the
device fail in a confusing way.

The protocol is separated from the bytes it rides on, so you can drive it over
something other than usbfs — gousb, libusb, usbip, or a fake in tests:

```go
type Transport interface {
    BulkOut(data []byte, timeout time.Duration) error
    BulkIn(buf []byte, timeout time.Duration) (int, error)
    Close() error
}

dev := cx4300.New(myTransport)
```

Exported building blocks, useful on their own: `BuildSetWindow`, `GammaTable`,
`Deinterleave`, `PlaneStride`, `WireSize`, and the `Status*` constants.
`go test ./cx4300` exercises the whole command sequence against a fake
transport, so it runs without hardware.

## Using the web UI

```sh
escan --addr 127.0.0.1:8080 --out ~/scans
```

**Preview** scans the whole bed at the resolution picked next to the button.
Drag on the preview to choose an area — the selection is shown in millimetres
and in output pixels — then **Scan selection** at the scan resolution, or
**Scan full bed**. **Preview selection** re-previews just the crop, which is
worth doing before committing to a slow high-resolution pass.

A scan never replaces the preview: the preview and the crop you drew stay put,
so you can change the resolution or nudge the area and scan again. The result
appears as a thumbnail in the side panel, and the **Last scan** / **Preview**
buttons switch the main view between them.

Scanning a selection is also quicker than the whole bed — the 300 dpi CD crop
below took 30 s against 105 s for the full platen.

Resolution is the only real lever on speed, because the time is dominated by
the carriage, not the USB link. Measured on a full bed:

| Resolution | Time | Pixels |
|---|---|---|
| 75 dpi | 8 s | 637 × 877 |
| 150 dpi | 27 s | 1275 × 1755 |
| 300 dpi | 105 s | 2550 × 3510 |
| 600 dpi | ~7 min (extrapolated) | 5100 × 7020 |

75 dpi is the lowest the device offers and is often good enough for a real
scan, not just a preview.

Finished scans are written to `--out` as PNG at full resolution. The copy sent
to the browser is scaled down so the page stays responsive — a 600 dpi full bed
is over 100 MB — while **Download PNG** always serves the full-resolution file
from disk.

Useful flags:

| Flag | Default | Meaning |
|---|---|---|
| `--preview-dpi` | `75` | resolution the preview selector starts on |
| `--preview-max` | `900` | longest edge of the preview sent to the browser |
| `--display-max` | `1600` | longest edge of a finished scan shown in the browser |

Setting either `--preview-max` or `--display-max` to `0` disables scaling.

Because a browser is the front end, it works over SSH to a headless machine
(forward the port) as well as on a desktop.

## Running it from WSL

`usbipd-win` hands the device to Linux, and it arrives on a virtual root hub,
which satisfies the no-hub rule. In an Administrator PowerShell:

```powershell
usbipd list
usbipd bind --force --busid <busid>    # --force if USBPcap is installed
usbipd attach --wsl --busid <busid>
```

`lsusb` in WSL should then show `04b8:083f` directly under a root hub. To hand
it back to Windows, `usbipd detach --busid <busid>` — and note that after a
detach Windows usually needs the USB cable physically replugged before it will
use the scanner again.

## Status and limitations

Verified on both platforms against real hardware: identify, 75 dpi preview,
cropped scans at 300 dpi and full-bed scans at 150 dpi. Linux and Windows
produce the same framing and the same output dimensions. Plane padding is
confirmed at 75, 150 and 300 dpi.

Not verified: 600 dpi. It should work, but a full-bed 600 dpi scan is ~107 MB
over this device's USB 1.1 link, so time it before assuming a timeout is a bug.

This is not a SANE backend, so XSane and GIMP cannot use it. Writing one around
`cx4300/` would be a reasonable next step — the protocol work is done.
