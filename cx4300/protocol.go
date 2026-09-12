package cx4300

import (
	"errors"
	"fmt"
	"image"
	"strings"
	"time"
)

// Timeouts. The status read is generous because the device reports a lamp
// warm-up time of up to 90 seconds and answers the status frame only once it is
// ready to proceed.
const (
	writeTimeout  = 20 * time.Second
	readTimeout   = 30 * time.Second
	statusTimeout = 120 * time.Second
	drainTimeout  = 700 * time.Millisecond
)

// imageBlock is the transfer length the Windows driver uses for each image
// READ(10). Keeping the same value avoids surprises on a device whose firmware
// is clearly particular about what it is asked.
const imageBlock = 0x01fe00

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
		n, err := d.t.BulkIn(buf, drainTimeout)
		if err != nil || n == 0 {
			return total
		}
		total += n
	}
}

func (d *Device) status() (byte, error) {
	buf := make([]byte, 8)
	n, err := d.t.BulkIn(buf, statusTimeout)
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
	if err := d.t.BulkOut(cdb, writeTimeout); err != nil {
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
		if err := d.t.BulkOut(send, writeTimeout); err != nil {
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
		for len(buf) < want {
			chunk := make([]byte, min(want-len(buf), 65536))
			n, err := d.t.BulkIn(chunk, readTimeout)
			if err != nil || n == 0 {
				break
			}
			buf = append(buf, chunk[:n]...)
		}
		if _, err := d.status(); err != nil {
			return buf, fmt.Errorf("%s: %w", name, err)
		}
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
	if err := p.Validate(); err != nil {
		return nil, err
	}
	width, height := p.Area.Pixels(p.DPI)
	total := WireSize(width, height)

	d.drain()

	// INQUIRY first: it is the cheapest way to find out the device is latched
	// before we start moving the carriage.
	short, err := d.command("INQUIRY", []byte{0x12, 0, 0, 0, 0x33, 0}, nil, 51)
	if err != nil {
		return nil, err
	}
	if len(short) == 0 {
		return nil, ErrLatched
	}

	if _, err := d.command("TEST UNIT READY", []byte{0x00, 0, 0, 0, 0, 0}, nil, 0); err != nil {
		return nil, err
	}
	if _, err := d.command("RESERVE UNIT", []byte{0x16, 0, 0, 0, 0, 0}, nil, 0); err != nil {
		return nil, err
	}
	defer d.command("RELEASE UNIT", []byte{0x17, 0, 0, 0, 0, 0}, nil, 0)

	win := BuildSetWindow(p.DPI, p.Area)
	setWindow := []byte{0x24, 0, 0, 0, 0, 0, 0, 0, byte(len(win)), 0}
	if _, err := d.command("SET WINDOW", setWindow, win, 0); err != nil {
		return nil, err
	}
	if _, err := d.command("INQUIRY(148)", []byte{0x12, 0, 0, 0, 0x94, 0}, nil, 148); err != nil {
		return nil, err
	}
	if _, err := d.command("WRITE gamma",
		[]byte{0x2a, 0, 0x03, 0, 0, 0x94, 0, 0x10, 0, 0}, GammaTable(), 0); err != nil {
		return nil, err
	}
	// Calibration/shading data. The contents are all zeros in practice, but the
	// device expects the exchange.
	if _, err := d.command("READ calibration",
		[]byte{0x28, 0, 0x80, 0, 0, 1, 0, 0xff, 0, 0}, nil, 0x00ff00); err != nil {
		return nil, err
	}
	if _, err := d.command("READ calibration end",
		[]byte{0x28, 0, 0x80, 0, 0, 1, 0, 0x00, 0, 0}, nil, 0); err != nil {
		return nil, err
	}

	if _, err := d.command("SCAN", []byte{0x1b, 0, 0, 0, 0, 0}, nil, 0); err != nil {
		return nil, err
	}

	raw := make([]byte, 0, total)
	for len(raw) < total {
		want := min(imageBlock, total-len(raw))
		cdb := []byte{0x28, 0, 0, 0, 0, 0,
			byte(want >> 16), byte(want >> 8), byte(want), 0}
		chunk, err := d.command("READ image", cdb, nil, want)
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			break
		}
		raw = append(raw, chunk...)
		if d.Progress != nil {
			d.Progress(len(raw), total)
		}
	}
	if len(raw) == 0 {
		return nil, errors.New("cx4300: scan returned no image data")
	}
	return Deinterleave(raw, width, height), nil
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
