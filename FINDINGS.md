# Diagnosis log

Chronological record of what was tried against the Epson Stylus CX4300 scanner
(`04b8:083f`), so none of it needs repeating.

## Conclusion

`epkowa` cannot drive this device as shipped. Its opening ESC/I probe (`1b 66`)
is an invalid SCSI CDB that latches the scanner into refusing every further
command until power cycled. The scanner itself is fine and speaks SCSI over
bulk — see `PROTOCOL.md`. A direct SCSI client works.

## Driver stack (correct, but not sufficient)

Epson's official packages do install and load correctly:

* `iscan_2.30.4-2` — `epkowa` SANE backend plus the non-free `libesmod` core
* `iscan-plugin-cx4400_2.1.4-1` — the `libesint7E` interpreter for this model
* `iscan-data_1.39.2-1`

From `iscan-cx4400-bundle-2.30.4.x64.deb.tar.gz`, sha256
`1033f226fe06f4422474d762292c3fee1cde59d18a35b969f60ad73090cf3278`.
The ISO's own `Es007e.sif` confirms the model mapping:
`ES007e="EPSON CX4300/CX5500/DX4400"` → `libesint7E`.

Two genuine Epson packaging bugs need fixing by hand on any install:

1. `/usr/share/iscan-data/usb` ships **without** a `usb 0x04b8 0x083f` line, so
   autodetection never finds the device. Add it.
2. `/etc/sane.d/epkowa.conf` needs an explicit `usb 0x04b8 0x083f`; a bare `usb`
   is not enough.

Also worth doing: trim `/etc/sane.d/dll.conf` to `epkowa` only. Other backends
probe the device with foreign commands and wedge it.

On Debian/Ubuntu the core packages need `dpkg -i --force-depends` (they ask for
the old `libsane` name). On Ubuntu 26.04 the backend also needs
`libxml2.so.2` — that release ships `libxml2.so.16`, and the two sonames
coexist safely, so dropping an older `libxml2.so.2.9.14` into
`/usr/lib/x86_64-linux-gnu/` and running `ldconfig` is enough.

## Ruled out, with evidence

| Suspect | Evidence against |
|---|---|
| USB cable | Scans perfectly on Windows with the same cable |
| Host / chipset generation | Reproduced identically on a 2010 Intel laptop and a modern Debian 13 box |
| USB controller type | Fails the same on EHCI and on xHCI |
| Distro / kernel age | Void with kernel 6.18 and Ubuntu 26.04 with kernel 7.0 behave alike |
| `usblp` contention | Blacklisted and unloaded; no change. WSL has no `usblp` at all |
| Install method | Manual file copy and official `dpkg` install behave identically |
| Missing interpreter plugin | `strace` confirms `libesint7E.so` is opened and used |
| Device readiness / lamp warm-up | `fb` stayed constant while polling every 8 s for 2 minutes |
| Printer side being offline | Printer-class `GET_PORT_STATUS` returns `0x18` (online) |
| A vendor control request | The Windows capture contains only `GET_DESCRIPTOR`, `SET_CONFIGURATION` and printer-class `GET_DEVICE_ID` |
| Printer-channel traffic | The successful Windows scan used **no** bulk traffic on ep `0x01`/`0x81` |
| URB ordering | Submitting the IN read before the CDB still returns `fb` |
| Old driver versions | Unobtainable: Epson's 1.0.0 bundle is 404 and every archived avasys `.deb` is a 302 with no stored content |

## USB hubs genuinely matter

The scanner's identify step fails when it sits behind a USB hub and succeeds
when attached directly to a root hub. Corroborated three ways: a DX4400 user on
askubuntu fixed their scanner by removing a hub; on Windows the scan failed
behind a Generic USB Hub and worked direct on the root hub; and `epkowa`
reported real geometry (`215.9 x 297.18 mm` instead of `0..0mm`) for the first
time once the device was passed into Linux with no intervening hub.

## Firmware lockups

The unit wedges easily and then ignores its own power button. Only removing
mains power clears that. Symptoms while wedged: it answers a canned `fb`
frame to everything, drops off the USB bus roughly 75 s after enumerating, and
eventually fails to enumerate at all with `device descriptor read/64, error -32`.
None of that is a Linux problem.
