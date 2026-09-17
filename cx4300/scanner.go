package cx4300

import (
	"fmt"
	"image"
	"time"
)

// USB identity of the scanner.
const (
	VendorID  = 0x04b8
	ProductID = 0x083f
)

// Geometry. The device measures the bed in 1/600 inch units regardless of the
// scan resolution, so all areas in this package use those units.
const (
	// Unit is the number of area units per inch.
	Unit = 600
	// BedWidth and BedHeight are the full platen, 8.5 x 11.7 inch.
	BedWidth  = 5100
	BedHeight = 7020
)

// Status bytes returned in the first byte of every 8-byte status frame.
const (
	StatusSend = 0xf8 // device expects the data-out phase
	StatusData = 0xf9 // data is available to read
	StatusDone = 0xfb // command complete, nothing to transfer
)

// Bulk endpoints on interface 0.
const (
	EndpointOut = 0x02
	EndpointIn  = 0x82
)

// SupportedDPI lists the resolutions the device reports. Other values are
// rejected by Params.Validate rather than silently rounded.
var SupportedDPI = []int{75, 150, 300, 600}

// Mode is the pixel format a scan is delivered in.
//
// The device itself only ever scans 24-bit colour - its SET WINDOW block asks
// for image composition 0x05 and the firmware offers no grayscale composition
// this driver has been able to get an image out of - so ModeGray is computed
// here from the colour the device sends, not requested from it. It still earns
// its place: a grey page scanned in colour carries the three channels' noise
// and any misregistration left between them as visible colour speckle, and the
// luma average cancels most of both. It is also a third of the bytes to the
// frontend, though not over the wire.
type Mode int

const (
	// ModeColor delivers 8-bit RGB, three bytes per pixel.
	ModeColor Mode = iota
	// ModeGray delivers 8-bit luma, one byte per pixel.
	ModeGray
)

// BytesPerPixel is how wide one pixel is in the delivered image.
func (m Mode) BytesPerPixel() int {
	if m == ModeGray {
		return 1
	}
	return 3
}

func (m Mode) String() string {
	if m == ModeGray {
		return "gray"
	}
	return "color"
}

// Area is a rectangle on the platen in 1/600 inch units.
type Area struct {
	X, Y, W, H int
}

// FullBed returns the whole platen.
func FullBed() Area { return Area{X: 0, Y: 0, W: BedWidth, H: BedHeight} }

// Pixels converts the area to output pixels at the given resolution.
func (a Area) Pixels(dpi int) (w, h int) {
	return a.W * dpi / Unit, a.H * dpi / Unit
}

// Millimetres converts the area's size to millimetres, for display.
func (a Area) Millimetres() (w, h float64) {
	const mmPerInch = 25.4
	return float64(a.W) / Unit * mmPerInch, float64(a.H) / Unit * mmPerInch
}

// OpticalDPI is the finest the sensor genuinely samples. Below it the device
// subsamples rather than averaging, and because the three colour planes read
// different rows they alias the fine detail differently - which comes out as
// colour fringing on thin, near-horizontal lines, worst at 75 dpi.
//
// Measured on line art, as the mean channel difference over one window with
// each channel's own level removed, for a 75 dpi result: 33.1 counts scanned
// natively, 21.2 from 150, 17.1 from 300, 17.0 from 600. So 300 is where it
// stops paying, and 600 as a source costs four times the sweep for nothing.
const OpticalDPI = 300

// Params describes one scan.
type Params struct {
	DPI  int
	Area Area
	Mode Mode

	// Oversample scans at OpticalDPI and averages down when the wanted
	// resolution is a whole fraction of it, which is the only way to get clean
	// colour below 300 dpi. It costs the time of the higher-resolution sweep,
	// so a preview - where framing matters and colour does not - leaves it off.
	Oversample bool
}

// sampling reports the resolution the device is actually driven at and how many
// source pixels fold into one output pixel on each axis. The fold has to be a
// whole number, so a resolution that does not divide OpticalDPI is scanned
// natively however Oversample is set.
func (p Params) sampling() (dpi, box int) {
	if !p.Oversample || p.DPI >= OpticalDPI || OpticalDPI%p.DPI != 0 {
		return p.DPI, 1
	}
	return OpticalDPI, OpticalDPI / p.DPI
}

// PixelSize is the size of the image a scan delivers. With oversampling it is
// the source size divided by the fold, not the area at the wanted resolution:
// deriving it from the source keeps the two from disagreeing by a pixel when
// the division is not exact.
func (p Params) PixelSize() (w, h int) {
	dpi, box := p.sampling()
	sw, sh := p.Area.Pixels(dpi)
	return sw / box, sh / box
}

// Validate reports whether the parameters are usable, so callers get a clear
// error instead of a wedged scanner.
func (p Params) Validate() error {
	ok := false
	for _, d := range SupportedDPI {
		if p.DPI == d {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("cx4300: unsupported resolution %d dpi (device supports %v)", p.DPI, SupportedDPI)
	}
	a := p.Area
	if a.W <= 0 || a.H <= 0 {
		return fmt.Errorf("cx4300: empty scan area %+v", a)
	}
	if a.X < 0 || a.Y < 0 || a.X+a.W > BedWidth || a.Y+a.H > BedHeight {
		return fmt.Errorf("cx4300: area %+v is outside the %dx%d bed", a, BedWidth, BedHeight)
	}
	if w, h := a.Pixels(p.DPI); w == 0 || h == 0 {
		return fmt.Errorf("cx4300: area %+v is smaller than one pixel at %d dpi", a, p.DPI)
	}
	if p.Mode != ModeColor && p.Mode != ModeGray {
		return fmt.Errorf("cx4300: unknown mode %d", int(p.Mode))
	}
	return nil
}

// DeviceInfo is what the scanner reports about itself.
type DeviceInfo struct {
	Model    string // e.g. "Color   Color MFP01     0119"
	Firmware string // build string from the long INQUIRY, when available
	Raw      []byte // the raw INQUIRY payload
}

// Scanner is the high-level interface. The Linux SCSI implementation (Device)
// and the Windows WIA implementation both satisfy it, so callers and the web
// UI are platform agnostic.
type Scanner interface {
	Identify() (DeviceInfo, error)
	Scan(Params) (image.Image, error)
	Close() error
}

// Transport moves bulk bytes to and from interface 0. Implement it to drive
// the protocol over something other than usbfs.
type Transport interface {
	BulkOut(data []byte, timeout time.Duration) error
	BulkIn(buf []byte, timeout time.Duration) (int, error)
	Close() error
}

// ProgressFunc is called during a scan as image data arrives.
type ProgressFunc func(done, total int)

// StreamScanner is implemented by backends that can deliver an image row by
// row as it is scanned, rather than only when it is complete. Only the Linux
// backend can: WIA hands over a finished file, so on Windows a caller has to
// wait for the whole scan. See Device.ScanRows.
type StreamScanner interface {
	ScanRows(p Params, fn func(y int, row []byte) error) (width, height int, err error)
}

// ProgressReporter is implemented by backends that can report scan progress.
// The Linux backend reports byte counts as image blocks arrive; the Windows WIA
// backend cannot see inside a transfer, so it only reports start and finish.
type ProgressReporter interface {
	SetProgress(ProgressFunc)
}

// Resetter is implemented by backends that can kick the device out of a stuck
// state without a power cycle. Only the Windows backend can do this, by
// disabling and re-enabling the PnP node.
type Resetter interface {
	Reset() error
}
