# Epson Stylus CX4300 scanner - USB wire protocol

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
5100 x 7020 is 8.5 x 11.7 inch - at 150 dpi that is 1275 x 1755 pixels, which
matches 51 image `READ(10)`s of 130560 bytes (6,712,875 bytes of RGB).

## Image data format

The image does **not** arrive as interleaved RGB pixels. Each scan line is sent
as three consecutive colour planes - the whole red row, then green, then blue -
and each plane is padded up to a multiple of **16 pixels**. The padding columns
must be cropped after deinterleaving.

Total bytes for a full-bed 150 dpi scan are therefore `1280 * 3 * 1755 =
6,739,200`, not `1275 * 1755 * 3`. Reading the smaller figure silently truncates
the last few lines.

**At 600 dpi, and only there, every plane carries one further 16-pixel block on
top of that rounding.** A 427-pixel line is sent as 432 at 300 dpi but as 448 at
600. The extra block trails the image, so the pixels still begin at column zero;
decoding a 600 dpi scan without it shears the image into coloured stripes.

Measured plane widths:

| Pixel width | Plane on the wire | Scan |
|---|---|---|
| 637 | 640 | 75 dpi full bed |
| 427 | 432 | 150 dpi crop |
| 1275 | 1280 | 150 dpi full bed |
| 400 | 400 | 300 dpi crop, already aligned |
| 427 | 432 | 300 dpi crop |
| 1460 | 1472 | 300 dpi crop |
| 1666 | **1680** | 300 dpi crop |
| 2550 | 2560 | 300 dpi full bed |
| 400 | **416** | 600 dpi crop, aligned yet padded anyway |
| 427 | **448** | 600 dpi crop |
| 448 | **464** | 600 dpi crop |
| 465 | **496** | 600 dpi crop |
| 500 | **528** | 600 dpi crop |
| 1200 | **1216** | 600 dpi crop |

The 16 is load-bearing and was measured, not inferred. Every width below 600 dpi
except 1666 pads identically under a 16-, 32- or 64-pixel rule, so those cases
cannot tell the rules apart; 1666 can, and it pads to 1680. Assuming 32 there
leaves each line 96 bytes short and shears the image progressively, while
assuming 64 asks the device for more data than it has and wedges it until mains
power is cut.

The 600 dpi rows rule out the obvious alternatives too: 465 pads to 496, which
is not a multiple of 32 or 64, and 400 and 448 are already multiples of 16 yet
still gain a block - so this is an extra block, not a coarser alignment. 1200
shows it is not an effect of small widths.

All of these were measured the same way: dump the bytes a scan actually returns
and recover the line period from them, by finding the byte offset at which the
buffer best correlates with itself. The period is the plane width times three.

### The three planes are not the same row

The three planes of one wire line do not describe the same row of the page. With
each plane's differenced row means aligned against red's, over two captures of a
document with table rules and text:

| | 300 dpi | 600 dpi |
|---|---|---|
| green trails red by | 0.15 rows | 0.18 rows |
| blue trails red by | 0.85 rows | 0.95 rows |

Both correlation curves peak cleanly (0.98 and 0.98 at 300 dpi, 0.99 and 1.00 at
600), and the planes share a pitch horizontally to within 0.03 pixels across a
row, so this is the only misregistration present. Blue is nearly a whole row
out.

The lag is in **lines, not inches**: a sensor whose three rows were physically
apart would double from 300 to 600 dpi, and it does not. So it is applied per
line at every resolution, as a linear mix of the wire line and the one above it.

Decoding without it leaves a colour fringe on every horizontal edge, and turns
the dither a printer renders grey with into a hue that rotates across the page -
a grey original coming back in false colour is the symptom to look for.
`tools/wirediag` measures all of this from a raw capture, and `escan calibrate`
measures it on the scanner in front of you and stores it - the lag is a property
of the individual unit, so the numbers above are this one's, not the model's.

### Below 300 dpi the device subsamples

300 dpi is the finest the sensor genuinely samples. Ask for less and the device
subsamples rather than averaging, and since the three planes read different rows
(above), each aliases fine detail differently. The result is colour fringing on
thin, near-horizontal lines: strong at 75 dpi, visible at 150, absent at 300 and
600.

Measured on line art as the mean channel difference over one window, each
channel's own level removed, for a 75 dpi result:

| source | 75 dpi | 150 dpi |
|---|---|---|
| scanned natively | 33.1 | 26.5 |
| from 150 dpi | 21.2 | - |
| from 300 dpi | **17.1** | **19.6** |
| from 600 dpi | 17.0 | 19.1 |

So a clean scan below 300 dpi is a 300 dpi scan averaged down by a whole factor,
and 600 as the source buys nothing for four times the sweep.

Because the expected total depends on this, treat a `READ` that returns fewer
bytes than requested as end-of-image and stop: issuing another `READ` leaves the
device mid-transfer, which is one of the ways it locks up.

The `READ` of `0x00ff00` = 65280 bytes before `SCAN` returns all zeros - it is
calibration/shading data, not a parameter block, so it cannot be used to learn
the line width.

## The trap that makes every Linux driver fail

SANE's `epkowa` backend (Epson's own Image Scan! driver, with the matching
non-free `libesint7E` interpreter plugin) opens by probing with ESC/I:

```
1b 66        ESC f - "request extended status"
```

This device never answers it - two 30-second timeouts - and, worse, the bytes
are an invalid SCSI CDB. Once the device receives them it **latches into
answering `fb` to every subsequent command** and stays that way until it is
power cycled. Re-plugging the cable and re-enumerating do not clear it.

That is why `epkowa` reports a `0..0mm` scan area and then fails
`sane_start: Invalid argument`: its own probe poisons the device before its
SCSI fallback ever runs.

Proven by experiment: on a freshly power-cycled device, speaking SCSI directly
returns `f9` plus valid INQUIRY data, byte-identical to the Windows capture.
After any `epkowa` run in the same power session, the same CDB returns `fb`.
