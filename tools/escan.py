#!/usr/bin/env python3
"""Scan from an Epson Stylus CX4300 (USB 04b8:083f).

The scanner interface speaks SCSI scanner CDBs over bulk endpoints, wrapped in a
one-byte status protocol: after each CDB the device returns an 8-byte status
frame whose first byte is 0xf8 (send the data-out phase), 0xf9 (data is ready to
read) or 0xfb (done).  Note that epkowa's opening ESC/I probe is an invalid CDB
and latches the device into answering 0xfb to everything until it is power
cycled, so nothing else may touch the device first.
"""
import ctypes, sys, time

OUT = "/tmp/cx4300.pnm"
VID, PID = 0x04b8, 0x083f
EP_OUT, EP_IN = 0x02, 0x82
ST_SEND, ST_DATA, ST_DONE = 0xf8, 0xf9, 0xfb

L = ctypes.CDLL("libusb-1.0.so.0")
L.libusb_open_device_with_vid_pid.restype = ctypes.c_void_p
L.libusb_open_device_with_vid_pid.argtypes = [ctypes.c_void_p, ctypes.c_uint16, ctypes.c_uint16]
for f in ("libusb_claim_interface", "libusb_detach_kernel_driver", "libusb_release_interface"):
    getattr(L, f).argtypes = [ctypes.c_void_p, ctypes.c_int]
L.libusb_clear_halt.argtypes = [ctypes.c_void_p, ctypes.c_ubyte]
L.libusb_bulk_transfer.argtypes = [ctypes.c_void_p, ctypes.c_ubyte,
                                   ctypes.POINTER(ctypes.c_ubyte), ctypes.c_int,
                                   ctypes.POINTER(ctypes.c_int), ctypes.c_uint]

ctx = ctypes.c_void_p()
L.libusb_init(ctypes.byref(ctx))
h = L.libusb_open_device_with_vid_pid(ctx, VID, PID)
if not h:
    sys.exit("scanner 04b8:083f not found")
L.libusb_detach_kernel_driver(h, 0)
if L.libusb_claim_interface(h, 0) != 0:
    sys.exit("cannot claim interface 0")
L.libusb_clear_halt(h, EP_OUT)
L.libusb_clear_halt(h, EP_IN)

def write(data, timeout=20000):
    b = (ctypes.c_ubyte * len(data))(*data)
    n = ctypes.c_int(0)
    r = L.libusb_bulk_transfer(h, EP_OUT, b, len(data), ctypes.byref(n), timeout)
    if r:
        raise IOError("bulk OUT failed rc=%d" % r)

def read(length, timeout=30000):
    rb = (ctypes.c_ubyte * length)()
    n = ctypes.c_int(0)
    r = L.libusb_bulk_transfer(h, EP_IN, rb, length, ctypes.byref(n), timeout)
    if r:
        return None
    return bytes(rb[:n.value])

def drain():
    """Flush data a previous aborted run may have left queued on bulk IN."""
    total = 0
    while True:
        chunk = read(65536, timeout=700)
        if not chunk:
            break
        total += len(chunk)
    if total:
        print("drained %d stale bytes" % total)

def status():
    s = read(8, timeout=120000)
    if s is None or not s:
        raise IOError("no status frame")
    return s[0]

def command(name, cdb, send=None, want=0):
    """Run one CDB, optionally with a data-out or data-in phase."""
    write(cdb)
    st = status()
    if st == ST_SEND:
        if send is None:
            raise IOError("%s: device asked for data we don't have" % name)
        write(send)
        st = status()
        if st != ST_DONE:
            print("  %s: trailing status 0x%02x" % (name, st))
        return None
    if st == ST_DATA:
        buf = b""
        while len(buf) < want:
            chunk = read(min(want - len(buf), 65536))
            if not chunk:
                break
            buf += chunk
        st = status()
        return buf
    if st != ST_DONE:
        print("  %s: unexpected status 0x%02x" % (name, st))
    return None

# Window parameters replayed from the Windows driver: 150 dpi, full bed,
# 8-bit RGB.  Sizes are in 1/600 inch, so 5100x7020 -> 1275x1755 pixels.
DPI = 150
WIN = bytes.fromhex(
    "00000000000000320000009600960000"
    "000000000000000013ec00001b6c0000"
    "00050800000700000000000000000000"
    "000140ffffff00000310")
WIDTH  = 5100 * DPI // 600          # 1275 px at 150 dpi
HEIGHT = 7020 * DPI // 600          # 1755 px at 150 dpi
# Each scan line arrives as three separate colour planes (all R, then all G,
# then all B), and each plane is padded up to a multiple of 32 pixels --
# 1275 becomes 1280.  So a line on the wire is PLANE * 3 bytes, not WIDTH * 3.
PLANE  = (WIDTH + 31) // 32 * 32
TOTAL  = PLANE * 3 * HEIGHT

drain()

# Gamma table for WRITE(10): 4096 entries, 8-bit output.  The leading bytes are
# replayed verbatim from the Windows driver; the tail follows the curve they
# fit, which is plain gamma 1.8 (round(255 * (i/4095) ** (1/1.8))).
GAMMA_HEAD = bytes.fromhex("00000102030304050606070809090a0b0c0c0c0c0d0d0d0e0e0e0f0f0f1010101111111212121213131314141415151516161616161717171717171818181818191919191a1a1a1a1b1b1b1b1c1c1c1c1d1d1d1d1d1e1e1e1e1e1e1f1f1f1f1f2020202020212121212122222222222223232323232323242424242424242425252525252526262626262727272727272828282828282829292929292929292a2a2a2a2a2a2a2a2b2b2b2b2b2b2b2b2c2c2c2c2c2c2d2d2d2d2d2e2e2e2e2e2e2f2f2f2f2f2f2f3030303030303030313131313131313132323232323232323333333333333333343434343434343435353535353535353636363636363636363737373737373737383838383838383839393939393939393939393939393a3a3a3a3a3a3a3a3a3a3b3b3b3b3b3b3b3b3c3c3c3c3c3c3c3c3d3d3d3d3d3d3d3d3e3e3e3e3e3e3e3e3f3f3f3f3f3f3f40404040404040404040404040404141414141414141414142424242424242424343434343434343444444444444444445454545454545454545454545454646464646464646464647474747474747474848484848484848484848484848494949494949494949494a4a4a4a4a4a4a4a4b4b4b4b4b4b4b4b4b4b4b4b4b4b4c4c4c4c4c4c4c4c4c4c4d4d4d4d4d4d4d4d4e4e4e4e4e4e")
GAMMA = bytearray(GAMMA_HEAD)
while len(GAMMA) < 4096:
    i = len(GAMMA)
    GAMMA.append(int(round(255.0 * (i / 4095.0) ** (1.0 / 1.8))))
GAMMA = bytes(GAMMA)

print("target image: %dx%d RGB, %d-px planes, %d bytes on the wire"
      % (WIDTH, HEIGHT, PLANE, TOTAL))
d = command("INQUIRY", [0x12, 0, 0, 0, 0x33, 0], want=51)
if not d:
    sys.exit("INQUIRY returned no data - device is latched; power cycle it "
             "and make sure nothing else (scanimage/epkowa) touches it first")
print("device: %s" % bytes(c if 32 <= c < 127 else 46 for c in d[8:36]).decode())

command("TEST UNIT READY", [0x00, 0, 0, 0, 0, 0])
command("RESERVE UNIT", [0x16, 0, 0, 0, 0, 0])
command("SET WINDOW", [0x24, 0, 0, 0, 0, 0, 0, 0, 0x3a, 0], send=WIN)
command("INQUIRY(148)", [0x12, 0, 0, 0, 0x94, 0], want=148)
command("WRITE gamma", [0x2a, 0, 0x03, 0, 0, 0x94, 0, 0x10, 0, 0], send=GAMMA)
command("READ params", [0x28, 0, 0x80, 0, 0, 1, 0, 0xff, 0, 0], want=0x00ff00)
command("READ params end", [0x28, 0, 0x80, 0, 0, 1, 0, 0x00, 0, 0], want=0)

print("starting scan...")
t0 = time.time()
command("SCAN", [0x1b, 0, 0, 0, 0, 0])

img = b""
block = 0x01fe00
while len(img) < TOTAL:
    want = min(block, TOTAL - len(img))
    chunk = command("READ image", [0x28, 0, 0, 0, 0, 0,
                                   (want >> 16) & 0xff, (want >> 8) & 0xff, want & 0xff, 0],
                    want=want)
    if not chunk:
        print("  read returned nothing at %d bytes" % len(img))
        break
    img += chunk
    print("  %d / %d bytes (%.0f%%)" % (len(img), TOTAL, 100.0 * len(img) / TOTAL))

command("RELEASE UNIT", [0x17, 0, 0, 0, 0, 0])
L.libusb_release_interface(h, 0)
print("scan finished in %.1fs, %d bytes" % (time.time() - t0, len(img)))

if img:
    stride = PLANE * 3
    lines = len(img) // stride
    out = bytearray(WIDTH * lines * 3)
    for row in range(lines):
        o = row * stride
        r_plane = img[o:o + PLANE]
        g_plane = img[o + PLANE:o + 2 * PLANE]
        b_plane = img[o + 2 * PLANE:o + 3 * PLANE]
        base = row * WIDTH * 3
        for x in range(WIDTH):
            out[base + x * 3]     = r_plane[x]
            out[base + x * 3 + 1] = g_plane[x]
            out[base + x * 3 + 2] = b_plane[x]
    with open(OUT, "wb") as f:
        f.write(b"P6\n%d %d\n255\n" % (WIDTH, lines))
        f.write(bytes(out))
    print("wrote %s (%dx%d)" % (OUT, WIDTH, lines))
