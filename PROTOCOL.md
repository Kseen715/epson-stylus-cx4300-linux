# Epson Stylus CX4300 scanner — USB wire protocol

USB ID `04b8:083f` (shared with CX4400 / CX5500 / CX5600 / DX4400 / DX4450).

Reverse engineered by capturing a working Windows scan with USBPcap and
comparing it against what Linux drivers send.

## Interfaces

| Interface | Class | Endpoints | Use |
|---|---|---|---|
| 0 | Vendor Specific (255) | `0x02` OUT, `0x82` IN | **scanner** |
| 1 | Printer (7) | `0x01` OUT, `0x81` IN | printer |

The IEEE-1284 device ID (printer-class `GET_DEVICE_ID`, `bmRequestType=0xa1`,
`bRequest=0`, `wIndex=0x0100`) reports:

```
MFG:EPSON;CMD:ESCPL2,BDC,D4,D4PX;MDL:Stylus CX4300;CLASS:PRINTER;DES:EPSON Stylus CX4300
```

Note it advertises **no ESC/I command set**. That is not an omission: the
scanner does not speak ESC/I at all.

## Transport

The scanner speaks **SCSI scanner CDBs** over the interface-0 bulk pair, wrapped
in a trivial status protocol. Write the CDB to `0x02`, then read an 8-byte
status frame from `0x82`. The first byte selects the phase:

| Status | Meaning | Next action |
|---|---|---|
| `0xf8` | device expects a data-out phase | write the data, then read status again |
| `0xf9` | data is available | read the payload, then read the trailing status |
| `0xfb` | done / no data | nothing |

A data-in payload may arrive split across several bulk transfers (for example a
148-byte INQUIRY reply came back as 128 + 20), so read until the requested
length is reached. `READ(10)` carries its transfer length as a **24-bit
big-endian** field in CDB bytes 6..8.

## Commands used for one scan

```
00 00 00 00 00 00                      TEST UNIT READY
12 00 00 00 33 00                      INQUIRY, 51 bytes
16 00 00 00 00 00                      RESERVE UNIT
24 00 00 00 00 00 00 00 3a 00          SET WINDOW, 58-byte data-out phase
12 00 00 00 94 00                      INQUIRY, 148 bytes
2a 00 03 00 00 94 00 10 00 00          WRITE(10), 4096-byte gamma table
28 00 80 00 00 01 00 ff 00 00          READ, 0x00ff00 = 65280 bytes of parameters
28 00 80 00 00 01 00 00 00 00          READ, length 0 (returns fb)
1b 00 00 00 00 00                      SCAN  <- carriage starts here
28 00 00 00 00 00 01 fe 00 00          READ(10), 0x01fe00 = 130560 bytes, repeated
17 00 00 00 00 00                      RELEASE UNIT
```

INQUIRY(51) returns `06 00 02 02 49 00 00 00` followed by ASCII
`"Color   Color MFP01     0119"`. INQUIRY(148) additionally carries the firmware
build string `"Thu Oct 12 2006 10:12"`.

## SET WINDOW parameter block

58 bytes as captured from the Windows driver (150 dpi, full bed, 8-bit RGB):

```
00000000000000320000009600960000
000000000000000013ec00001b6c0000
00050800000700000000000000000000
000140ffffff00000310
```

Layout:

| Offset | Bytes | Meaning |
|---|---|---|
| 0..5 | `00 x6` | reserved |
| 6..7 | `00 32` | window descriptor length = 50 |
| 8..9 | `00 00` | window id, reserved |
| 10..11 | `00 96` | X resolution = 150 dpi |
| 12..13 | `00 96` | Y resolution = 150 dpi |
| 14..17 | `00000000` | upper-left X |
| 18..21 | `00000000` | upper-left Y |
| 22..25 | `000013ec` | width = 5100 |
| 26..29 | `00001b6c` | height = 7020 |
| 33 | `05` | image composition (RGB colour) |
| 34 | `08` | bits per pixel |

Width and height are in **1/600 inch**, independent of the resolution fields, so
5100 x 7020 is 8.5 x 11.7 inch — at 150 dpi that is 1275 x 1755 pixels, which
matches 51 image `READ(10)`s of 130560 bytes (6,712,875 bytes of RGB).

## Image data format

The image does **not** arrive as interleaved RGB pixels. Each scan line is sent
as three consecutive colour planes — the whole red row, then green, then blue —
and each plane is padded up to a multiple of 32 pixels. At 150 dpi a 1275-pixel
line is padded to 1280, so one line occupies `1280 * 3 = 3840` bytes and the
padding columns must be cropped after deinterleaving.

Total bytes for a full-bed 150 dpi scan are therefore `1280 * 3 * 1755 =
6,739,200`, not `1275 * 1755 * 3`. Reading the smaller figure silently truncates
the last few lines.

(The 32-pixel padding rule is inferred from a single resolution; it may really
be a byte-boundary rule. Worth re-checking if other resolutions look sheared.)

The `READ` of `0x00ff00` = 65280 bytes before `SCAN` returns all zeros — it is
calibration/shading data, not a parameter block, so it cannot be used to learn
the line width.

## The trap that makes every Linux driver fail

SANE's `epkowa` backend (Epson's own Image Scan! driver, with the matching
non-free `libesint7E` interpreter plugin) opens by probing with ESC/I:

```
1b 66        ESC f - "request extended status"
```

This device never answers it — two 30-second timeouts — and, worse, the bytes
are an invalid SCSI CDB. Once the device receives them it **latches into
answering `fb` to every subsequent command** and stays that way until it is
power cycled. Re-plugging the cable and re-enumerating do not clear it.

That is why `epkowa` reports a `0..0mm` scan area and then fails
`sane_start: Invalid argument`: its own probe poisons the device before its
SCSI fallback ever runs.

Proven by experiment: on a freshly power-cycled device, speaking SCSI directly
returns `f9` plus valid INQUIRY data, byte-identical to the Windows capture.
After any `epkowa` run in the same power session, the same CDB returns `fb`.
