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
| `install.sh` / `install.ps1` | installers (`--service` / `-Service` also runs it in the background) |
| `examples/` | systemd units and the Windows logon task, ready to edit by hand |
| [PROTOCOL.md](PROTOCOL.md) | the wire protocol, in detail |
| [FINDINGS.md](FINDINGS.md) | how it was worked out, and everything ruled out |
| `tools/escan.py` | the original Python proof of concept, kept as reference |
| `image/` | UI screenshots used below (`image/raw/` holds the uncropped originals) |

## The interface

The whole flow, in order: preview, drag a crop on it, then scan the selection at
whatever resolution you want.

**Nothing scanned yet.** The scanner identifies itself in the masthead, so you
know it is reachable before starting.

![Empty](image/01-empty.png)

**Preview running.** Progress is reported in megabytes as the data arrives; a
75 dpi full bed takes about 8 seconds.

![Preview running](image/02-preview-running.png)

**Preview.** The whole platen at 75 dpi. This is what you drag on.

![Preview](image/03-preview.png)

**Area selected.** Everything outside the marquee is dimmed, and the panel
reports the selection in millimetres, the output size in pixels at the chosen
resolution, and its origin on the glass.

![Selection](image/04-selection.png)

**Scan running.** The preview and the selection stay put while the scan runs, so
the area can be adjusted and re-scanned without previewing again.

![Scan running](image/05-scan-running.png)

**Finished.** The result is saved to disk at full resolution and thumbnailed in
the side panel; the preview is still there with the crop intact.

![Scan done](image/06-scan-done.png)

**Last scan.** Switching the view shows the result at full width. The preview is
one click away, selection unchanged.

![Last scan](image/07-last-scan.png)

## Install

Linux:

```sh
sudo ./install.sh
escan                      # then open http://127.0.0.1:8080/
```

The installer builds the binary, adds a udev rule so the `scanner` group can use
the device without root, and **disables the `epkowa` SANE backend** - see the
warning below. The finished binary has no runtime dependencies: no libusb, no
Python, no SANE.

Windows:

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1
escan
```

### Running it as a service

Optional - it is a foreground program by default. Pass the flag and it keeps
serving `http://127.0.0.1:8080/` from boot (Linux) or from logon (Windows),
writing scans to `~/scans`:

```sh
sudo ./install.sh --service        # systemd unit, runs as the user who invoked sudo
```

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1 -Service
```

Windows has no user-session service, so there it is a Scheduled Task that starts
at logon; WIA scanning needs the interactive session anyway.

The unit files are in [`examples/systemd/`](examples/systemd/) - the system-wide
`escan.service` the installer renders, and `escan.user.service` for a per-user
one that starts and stops with your login. The Windows task definition is
[`examples/windows/escan-logon-task.xml`](examples/windows/escan-logon-task.xml).

```sh
systemctl status escan       # is it up
journalctl -u escan -f       # what it is doing
sudo systemctl disable --now escan
```

### Configuration file

escan reads `/etc/escan.conf` at startup if it exists (`C:\ProgramData\escan\escan.conf`
on Windows, `--config` elsewhere). Every key is also a command-line flag, and a
flag given explicitly wins over the file. An unknown key is refused at startup
rather than ignored. The commented template is [`examples/escan.conf`](examples/escan.conf),
which `install.sh --service` drops in for you at mode 0600.

Because flags win, **the systemd unit passes none** - everything, the listen
address included, comes from the config file, so changing a setting is an edit
there and `systemctl restart escan`. A unit carrying `--addr` would quietly
ignore the `addr` line in the file. `WorkingDirectory` in the unit is what `out`
falls back to.

```ini
# /etc/escan.conf
addr = 0.0.0.0:8080     # reachable from the LAN; see the warning below
out  = /home/you/scans
```

escan has no authentication: anything that can reach the address can scan, and
can download everything in the output directory. `127.0.0.1:8080` is the
default for that reason - widen it only on a network you trust.

### Writing scans to a Samba share

Set an SMB address in the config file and escan writes there itself - no cifs
mount, no `mount.cifs`, no root, and no mount unit to order the service after:

```ini
# /etc/escan.conf, mode 0600
smb-address  = //nas.lan/scans/cx4300
smb-user     = scanuser
smb-password = secret
smb-domain   = WORKGROUP
```

The address is `//host/share`, optionally `//host:port/share/subdir`; the
subdirectory must already exist. `out` is ignored while `smb-address` is set,
and the Download link reads the file back off the share.

Two things worth knowing:

- **The password is config-file only.** There is deliberately no
  `--smb-password` flag - an argument is visible to every user on the machine
  through `/proc`. escan refuses to start if the file holding it is readable by
  anyone but its owner; `chmod 600 /etc/escan.conf`.
- **The share is proven at startup.** A wrong address, password or share name
  fails immediately instead of after a scan that took minutes. With
  `Restart=on-failure` in the unit, a NAS that is still booting is retried.

## Two things this scanner insists on

Both cost a lot of debugging time, and neither is a software bug.

**Never let SANE touch it.** `epkowa` opens by probing with ESC/I (`1b 66`).
This device does not implement ESC/I, and that probe is an invalid SCSI command
which latches the scanner into refusing *everything* until mains power is
removed. A single `scanimage -L` is enough. Re-plugging USB does not clear it.
`install.sh` disables the backend for you; the library reports this state as
`ErrLatched`.

**Plug it straight into a root-hub port.** Behind any USB hub its identify step
fails and the scan area reads back as zero. Internal hubs count - Intel
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
something other than usbfs - gousb, libusb, usbip, or a fake in tests:

```go
type Transport interface {
    BulkOut(data []byte, timeout time.Duration) error
    BulkIn(buf []byte, timeout time.Duration) (int, error)
    Close() error
}

dev := cx4300.New(myTransport)
```

Exported building blocks, useful on their own: `BuildSetWindow`, `GammaTable`,
`Deinterleave`, `PlaneStride`, `WireSize`, and the `Status*` constants. The
timeouts and the image block size are exported variables
(`cx4300.StatusTimeout`, `cx4300.ImageBlockSize`, …) so they can be tuned
without forking the package.
`go test ./cx4300` exercises the whole command sequence against a fake
transport, so it runs without hardware.

## Using the web UI

```sh
escan --addr 127.0.0.1:8080 --out ~/scans
```

**Preview** scans the whole bed at the resolution picked next to the button.
Drag on the preview to choose an area - the selection is shown in millimetres
and in output pixels - then **Scan selection** at the scan resolution, or
**Scan full bed**. **Preview selection** re-previews just the crop, which is
worth doing before committing to a slow high-resolution pass.

A scan never replaces the preview: the preview and the crop you drew stay put,
so you can change the resolution or nudge the area and scan again. The result
appears as a thumbnail in the side panel, and the **Last scan** / **Preview**
buttons switch the main view between them.

Scanning a selection is also quicker than the whole bed - the 300 dpi CD crop
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
to the browser is scaled down so the page stays responsive - a 600 dpi full bed
is over 100 MB - while **Download PNG** always serves the full-resolution file
from disk.

Useful flags:

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `127.0.0.1:8080` | address to listen on |
| `--out` | `.` | where finished scans are written |
| `--preview-dpi` | `75` | resolution the preview selector starts on |
| `--scan-dpi` | `300` | resolution preselected in the scan menu |
| `--preview-max` | `900` | longest edge of the preview sent to the browser |
| `--display-max` | `1600` | longest edge of a finished scan shown in the browser |

Setting either `--preview-max` or `--display-max` to `0` disables scaling.

To change the defaults themselves rather than pass flags, they are collected in
one block at the top of [cmd/escan/main.go](cmd/escan/main.go) (`defaultAddr`,
`defaultScanDPI` and friends). The browser-side equivalents - fallback
resolutions, the crop marquee's colours and dash pattern, the progress poll
interval - are in a single `CONFIG` object at the top of
[cmd/escan/web/app.js](cmd/escan/web/app.js). The server's values win wherever
it reports them, so `CONFIG` is only the fallback used before `/api/status`
answers.

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
it back to Windows, `usbipd detach --busid <busid>` - and note that after a
detach Windows usually needs the USB cable physically replugged before it will
use the scanner again.

## Status and limitations

Verified on both platforms against real hardware: identify, 75 dpi preview,
cropped scans at 300 dpi and full-bed scans at 150 dpi. Linux and Windows
produce the same framing and the same output dimensions. The plane padding rule
(a multiple of 16 pixels) was measured from raw wire data at five different
widths, including a crop width that distinguishes it from 32 and 64 - see
[PROTOCOL.md](PROTOCOL.md).

Not verified: 600 dpi. It should work, but a full-bed 600 dpi scan is ~107 MB
over this device's USB 1.1 link, so time it before assuming a timeout is a bug.

This is not a SANE backend, so XSane and GIMP cannot use it. Writing one around
`cx4300/` would be a reasonable next step - the protocol work is done.
