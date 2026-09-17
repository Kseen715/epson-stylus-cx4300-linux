package cx4300

import (
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
	"time"
)

// Tunables, deliberately variables rather than constants so they can be
// adjusted before a scan without editing this package.
//
// StatusTimeout is generous because the device reports a lamp warm-up time of
// up to 90 seconds and only answers the status frame once it is ready to
// proceed. ImageBlockSize is the transfer length the Windows driver uses for
// each image READ(10); keeping the same value avoids surprises on a device
// whose firmware is clearly particular about what it is asked, so change it
// only if you have a reason.
var (
	WriteTimeout   = 20 * time.Second
	ReadTimeout    = 30 * time.Second
	StatusTimeout  = 120 * time.Second
	DrainTimeout   = 700 * time.Millisecond
	ImageBlockSize = 0x01fe00
)

// ErrLatched reports the failure mode described in the package documentation:
// the device has been fed a malformed CDB (almost always epkowa's ESC/I probe)
// and now refuses every command until it is power cycled.
var ErrLatched = errors.New("cx4300: scanner is not answering commands (it reports " +
	"\"no data\" to INQUIRY). Something sent it an invalid command - a single " +
	"\"scanimage -L\" with epkowa enabled does this. Cut mains power to the " +
	"scanner for ~30s, then retry without letting any SANE tool touch it")

// Device implements Scanner by speaking the protocol over a Transport.
type Device struct {
	t Transport

	// Progress, when set, is called as image data arrives during Scan.
	Progress ProgressFunc
}

// New wraps a Transport. Use Open to get one backed by usbfs on Linux.
func New(t Transport) *Device { return &Device{t: t} }

// SetProgress implements ProgressReporter.
func (d *Device) SetProgress(fn ProgressFunc) { d.Progress = fn }

// Close releases the transport.
func (d *Device) Close() error { return d.t.Close() }

// drain discards anything a previously aborted run left queued on bulk IN.
// Without this, a run that died mid-transfer desynchronises every later
// command.
func (d *Device) drain() int {
	buf := make([]byte, 65536)
	total := 0
	for {
		n, err := d.t.BulkIn(buf, DrainTimeout)
		if err != nil || n == 0 {
			return total
		}
		total += n
	}
}

func (d *Device) status() (byte, error) { return d.statusWithin(StatusTimeout) }

func (d *Device) statusWithin(timeout time.Duration) (byte, error) {
	buf := make([]byte, 8)
	n, err := d.t.BulkIn(buf, timeout)
	if err != nil {
		return 0, fmt.Errorf("reading status frame: %w", err)
	}
	if n == 0 {
		return 0, errors.New("empty status frame")
	}
	return buf[0], nil
}

// command runs one CDB. send supplies the data-out phase for commands that ask
// for one; want is how many bytes to expect when the device offers data.
func (d *Device) command(name string, cdb []byte, send []byte, want int) ([]byte, error) {
	if err := d.t.BulkOut(cdb, WriteTimeout); err != nil {
		return nil, fmt.Errorf("%s: sending CDB: %w", name, err)
	}
	st, err := d.status()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	switch st {
	case StatusSend:
		if send == nil {
			return nil, fmt.Errorf("%s: device asked for a data-out phase we do not have", name)
		}
		if err := d.t.BulkOut(send, WriteTimeout); err != nil {
			return nil, fmt.Errorf("%s: sending data: %w", name, err)
		}
		if st, err = d.status(); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if st != StatusDone {
			return nil, fmt.Errorf("%s: unexpected trailing status 0x%02x", name, st)
		}
		return nil, nil

	case StatusData:
		buf := make([]byte, 0, want)
		short := false
		for len(buf) < want {
			chunk := make([]byte, min(want-len(buf), 65536))
			n, err := d.t.BulkIn(chunk, ReadTimeout)
			if err != nil || n == 0 {
				short = true
				break
			}
			buf = append(buf, chunk[:n]...)
		}
		// After a short read the trailing status may never arrive, so do not
		// block on it for the full status timeout.
		st, err := d.statusWithin(ReadTimeout)
		if err != nil && !short {
			return buf, fmt.Errorf("%s: %w", name, err)
		}
		_ = st
		return buf, nil

	case StatusDone:
		return nil, nil

	default:
		return nil, fmt.Errorf("%s: unexpected status 0x%02x", name, st)
	}
}

// Identify returns what the scanner reports about itself. It doubles as a
// liveness check: a device in the latched state fails here with ErrLatched.
func (d *Device) Identify() (DeviceInfo, error) {
	d.drain()
	short, err := d.command("INQUIRY", []byte{0x12, 0, 0, 0, 0x33, 0}, nil, 51)
	if err != nil {
		return DeviceInfo{}, err
	}
	if len(short) == 0 {
		return DeviceInfo{}, ErrLatched
	}
	info := DeviceInfo{Raw: short}
	if len(short) >= 36 {
		info.Model = strings.TrimSpace(printable(short[8:36]))
	}
	if long, err := d.command("INQUIRY(148)", []byte{0x12, 0, 0, 0, 0x94, 0}, nil, 148); err == nil && len(long) > 36 {
		info.Raw = long
		info.Firmware = longestPrintable(long[36:], 10)
	}
	return info, nil
}

// Scan performs one scan and returns the image. The caller must not run two
// scans concurrently against the same device; concurrent access wedges it.
func (d *Device) Scan(p Params) (image.Image, error) {
	raw, width, height, err := d.ScanRaw(p)
	if err != nil {
		return nil, err
	}
	return Deinterleave(raw, width, height), nil
}

// ScanRaw performs one scan and returns the bytes exactly as the device sent
// them, along with the pixel dimensions that were requested. The data is
// planar and padded - see Deinterleave, which Scan applies for you. Use this
// when you need the wire format itself, for instance to check the plane stride.
func (d *Device) ScanRaw(p Params) (raw []byte, width, height int, err error) {
	if err := p.Validate(); err != nil {
		return nil, 0, 0, err
	}
	width, height = p.Area.Pixels(p.DPI)
	raw = make([]byte, 0, WireSize(width, height))
	err = d.scan(p, func(chunk []byte) error {
		raw = append(raw, chunk...)
		return nil
	})
	if err != nil {
		return nil, 0, 0, err
	}
	return raw, width, height, nil
}

// ScanTo performs one scan and writes it to w as the data arrives, one row of
// 8-bit RGB triples after another, with the wire format's colour planes
// interleaved and its padding columns removed. It returns the pixel width of a
// row and how many rows were written, which is the requested height unless the
// device ended the image early.
//
// Use this rather than Scan when the image is consumed as a stream - a SANE
// frontend, a file, a socket - so that neither the raw nor the decoded image is
// ever held in memory in full. w must keep up: this blocks while it writes, and
// the device tolerates the pause between image blocks.
func (d *Device) ScanTo(p Params, w io.Writer) (width, height int, err error) {
	if err := p.Validate(); err != nil {
		return 0, 0, err
	}
	width, _ = p.Area.Pixels(p.DPI)
	rows := newRowWriter(w, width)
	if err := d.scan(p, rows.write); err != nil {
		return 0, 0, err
	}
	return width, rows.lines, nil
}

// scan runs the command sequence for one scan, handing each block of image data
// to sink as it arrives. Both ScanRaw and ScanTo are this loop with a different
// sink; nothing else should speak to the device while it runs.
func (d *Device) scan(p Params, sink func(chunk []byte) error) error {
	if err := p.Validate(); err != nil {
		return err
	}
	width, height := p.Area.Pixels(p.DPI)
	total := WireSize(width, height)

	d.drain()

	// INQUIRY first: it is the cheapest way to find out the device is latched
	// before we start moving the carriage.
	short, err2 := d.command("INQUIRY", []byte{0x12, 0, 0, 0, 0x33, 0}, nil, 51)
	if err2 != nil {
		return err2
	}
	if len(short) == 0 {
		return ErrLatched
	}

	if _, err := d.command("TEST UNIT READY", []byte{0x00, 0, 0, 0, 0, 0}, nil, 0); err != nil {
		return err
	}
	if _, err := d.command("RESERVE UNIT", []byte{0x16, 0, 0, 0, 0, 0}, nil, 0); err != nil {
		return err
	}
	defer d.command("RELEASE UNIT", []byte{0x17, 0, 0, 0, 0, 0}, nil, 0)

	win := BuildSetWindow(p.DPI, p.Area)
	setWindow := []byte{0x24, 0, 0, 0, 0, 0, 0, 0, byte(len(win)), 0}
	if _, err := d.command("SET WINDOW", setWindow, win, 0); err != nil {
		return err
	}
	if _, err := d.command("INQUIRY(148)", []byte{0x12, 0, 0, 0, 0x94, 0}, nil, 148); err != nil {
		return err
	}
	if _, err := d.command("WRITE gamma",
		[]byte{0x2a, 0, 0x03, 0, 0, 0x94, 0, 0x10, 0, 0}, GammaTable(), 0); err != nil {
		return err
	}
	// Calibration/shading data. The contents are all zeros in practice, but the
	// device expects the exchange.
	if _, err := d.command("READ calibration",
		[]byte{0x28, 0, 0x80, 0, 0, 1, 0, 0xff, 0, 0}, nil, 0x00ff00); err != nil {
		return err
	}
	if _, err := d.command("READ calibration end",
		[]byte{0x28, 0, 0x80, 0, 0, 1, 0, 0x00, 0, 0}, nil, 0); err != nil {
		return err
	}

	if _, err := d.command("SCAN", []byte{0x1b, 0, 0, 0, 0, 0}, nil, 0); err != nil {
		return err
	}

	got := 0
	for got < total {
		want := min(ImageBlockSize, total-got)
		cdb := []byte{0x28, 0, 0, 0, 0, 0,
			byte(want >> 16), byte(want >> 8), byte(want), 0}
		chunk, err := d.command("READ image", cdb, nil, want)
		if err != nil {
			return err
		}
		if len(chunk) == 0 {
			break
		}
		got += len(chunk)
		if err := sink(chunk); err != nil {
			return err
		}
		if d.Progress != nil {
			d.Progress(got, total)
		}
		if len(chunk) < want {
			// The device had less than we asked for, so the image is complete
			// even if our expected total said otherwise. Asking again would
			// leave the device mid-transfer, which wedges it until mains power
			// is cut.
			break
		}
	}
	if got == 0 {
		return errors.New("cx4300: scan returned no image data")
	}
	return nil
}

func printable(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 32 && c < 127 {
			out[i] = c
		} else {
			out[i] = ' '
		}
	}
	return string(out)
}

// longestPrintable returns the longest run of printable ASCII at least min
// bytes long, which is how the firmware build string is picked out of the long
// INQUIRY payload.
func longestPrintable(b []byte, minLen int) string {
	best, cur := "", strings.Builder{}
	flush := func() {
		if cur.Len() >= minLen && cur.Len() > len(best) {
			best = cur.String()
		}
		cur.Reset()
	}
	for _, c := range b {
		if c >= 32 && c < 127 {
			cur.WriteByte(c)
		} else {
			flush()
		}
	}
	flush()
	return strings.TrimSpace(best)
}
