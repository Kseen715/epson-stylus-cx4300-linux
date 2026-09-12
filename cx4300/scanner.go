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

// Params describes one scan.
type Params struct {
	DPI  int
	Area Area
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
