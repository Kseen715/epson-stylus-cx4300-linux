package cx4300

import (
	"encoding/binary"
	"image"
	"math"
)

// setWindowTemplate is the 58-byte SET WINDOW parameter block captured from the
// Windows driver. BuildSetWindow patches the resolution and area fields; every
// other byte is replayed verbatim, including image composition 0x05 (8-bit RGB)
// at offset 33, bits-per-pixel 0x08 at offset 34, and a vendor-specific tail
// whose meaning is not known.
var setWindowTemplate = []byte{
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x32, 0x00, 0x00, 0x00, 0x96,
	0x00, 0x96, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x13, 0xec, 0x00, 0x00, 0x1b, 0x6c, 0x00, 0x00, 0x00, 0x05, 0x08, 0x00,
	0x00, 0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x01, 0x40, 0xff, 0xff, 0xff, 0x00, 0x00, 0x03, 0x10,
}

// Field offsets inside the SET WINDOW parameter block.
const (
	swDescLen = 6  // uint16be, always 50
	swXRes    = 10 // uint16be, dpi
	swYRes    = 12 // uint16be, dpi
	swULX     = 14 // uint32be, 1/600 inch
	swULY     = 18 // uint32be
	swWidth   = 22 // uint32be
	swHeight  = 26 // uint32be
)

// gammaHead is the leading part of the gamma table as the Windows driver sends
// it. The remainder follows the curve these bytes fit, which is plain gamma
// 1.8, so the table is reconstructed rather than stored in full.
var gammaHead = []byte{
	0x00, 0x00, 0x01, 0x02, 0x03, 0x03, 0x04, 0x05, 0x06, 0x06, 0x07, 0x08,
	0x09, 0x09, 0x0a, 0x0b, 0x0c, 0x0c, 0x0c, 0x0c, 0x0d, 0x0d, 0x0d, 0x0e,
	0x0e, 0x0e, 0x0f, 0x0f, 0x0f, 0x10, 0x10, 0x10, 0x11, 0x11, 0x11, 0x12,
	0x12, 0x12, 0x12, 0x13, 0x13, 0x13, 0x14, 0x14, 0x14, 0x15, 0x15, 0x15,
	0x16, 0x16, 0x16, 0x16, 0x16, 0x17, 0x17, 0x17, 0x17, 0x17, 0x17, 0x18,
	0x18, 0x18, 0x18, 0x18, 0x19, 0x19, 0x19, 0x19, 0x1a, 0x1a, 0x1a, 0x1a,
	0x1b, 0x1b, 0x1b, 0x1b, 0x1c, 0x1c, 0x1c, 0x1c, 0x1d, 0x1d, 0x1d, 0x1d,
	0x1d, 0x1e, 0x1e, 0x1e, 0x1e, 0x1e, 0x1e, 0x1f, 0x1f, 0x1f, 0x1f, 0x1f,
	0x20, 0x20, 0x20, 0x20, 0x20, 0x21, 0x21, 0x21, 0x21, 0x21, 0x22, 0x22,
	0x22, 0x22, 0x22, 0x22, 0x23, 0x23, 0x23, 0x23, 0x23, 0x23, 0x23, 0x24,
	0x24, 0x24, 0x24, 0x24, 0x24, 0x24, 0x24, 0x25, 0x25, 0x25, 0x25, 0x25,
	0x25, 0x26, 0x26, 0x26, 0x26, 0x26, 0x27, 0x27, 0x27, 0x27, 0x27, 0x27,
	0x28, 0x28, 0x28, 0x28, 0x28, 0x28, 0x28, 0x29, 0x29, 0x29, 0x29, 0x29,
	0x29, 0x29, 0x29, 0x2a, 0x2a, 0x2a, 0x2a, 0x2a, 0x2a, 0x2a, 0x2a, 0x2b,
	0x2b, 0x2b, 0x2b, 0x2b, 0x2b, 0x2b, 0x2b, 0x2c, 0x2c, 0x2c, 0x2c, 0x2c,
	0x2c, 0x2d, 0x2d, 0x2d, 0x2d, 0x2d, 0x2e, 0x2e, 0x2e, 0x2e, 0x2e, 0x2e,
	0x2f, 0x2f, 0x2f, 0x2f, 0x2f, 0x2f, 0x2f, 0x30, 0x30, 0x30, 0x30, 0x30,
	0x30, 0x30, 0x30, 0x31, 0x31, 0x31, 0x31, 0x31, 0x31, 0x31, 0x31, 0x32,
	0x32, 0x32, 0x32, 0x32, 0x32, 0x32, 0x32, 0x33, 0x33, 0x33, 0x33, 0x33,
	0x33, 0x33, 0x33, 0x34, 0x34, 0x34, 0x34, 0x34, 0x34, 0x34, 0x34, 0x35,
	0x35, 0x35, 0x35, 0x35, 0x35, 0x35, 0x35, 0x36, 0x36, 0x36, 0x36, 0x36,
	0x36, 0x36, 0x36, 0x36, 0x37, 0x37, 0x37, 0x37, 0x37, 0x37, 0x37, 0x37,
	0x38, 0x38, 0x38, 0x38, 0x38, 0x38, 0x38, 0x38, 0x39, 0x39, 0x39, 0x39,
	0x39, 0x39, 0x39, 0x39, 0x39, 0x39, 0x39, 0x39, 0x39, 0x39, 0x3a, 0x3a,
	0x3a, 0x3a, 0x3a, 0x3a, 0x3a, 0x3a, 0x3a, 0x3a, 0x3b, 0x3b, 0x3b, 0x3b,
	0x3b, 0x3b, 0x3b, 0x3b, 0x3c, 0x3c, 0x3c, 0x3c, 0x3c, 0x3c, 0x3c, 0x3c,
	0x3d, 0x3d, 0x3d, 0x3d, 0x3d, 0x3d, 0x3d, 0x3d, 0x3e, 0x3e, 0x3e, 0x3e,
	0x3e, 0x3e, 0x3e, 0x3e, 0x3f, 0x3f, 0x3f, 0x3f, 0x3f, 0x3f, 0x3f, 0x40,
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40,
	0x40, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x42,
	0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x43, 0x43, 0x43, 0x43, 0x43,
	0x43, 0x43, 0x43, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x45,
	0x45, 0x45, 0x45, 0x45, 0x45, 0x45, 0x45, 0x45, 0x45, 0x45, 0x45, 0x45,
	0x45, 0x46, 0x46, 0x46, 0x46, 0x46, 0x46, 0x46, 0x46, 0x46, 0x46, 0x47,
	0x47, 0x47, 0x47, 0x47, 0x47, 0x47, 0x47, 0x48, 0x48, 0x48, 0x48, 0x48,
	0x48, 0x48, 0x48, 0x48, 0x48, 0x48, 0x48, 0x48, 0x48, 0x49, 0x49, 0x49,
	0x49, 0x49, 0x49, 0x49, 0x49, 0x49, 0x49, 0x4a, 0x4a, 0x4a, 0x4a, 0x4a,
	0x4a, 0x4a, 0x4a, 0x4b, 0x4b, 0x4b, 0x4b, 0x4b, 0x4b, 0x4b, 0x4b, 0x4b,
	0x4b, 0x4b, 0x4b, 0x4b, 0x4b, 0x4c, 0x4c, 0x4c, 0x4c, 0x4c, 0x4c, 0x4c,
	0x4c, 0x4c, 0x4c, 0x4d, 0x4d, 0x4d, 0x4d, 0x4d, 0x4d, 0x4d, 0x4d, 0x4e,
	0x4e, 0x4e, 0x4e, 0x4e, 0x4e,
}

// GammaTableSize is the length the device expects for the WRITE(10) gamma
// download.
const GammaTableSize = 4096

// BuildSetWindow renders the SET WINDOW parameter block for a scan. The device
// takes the area in 1/600 inch units independently of the resolution.
func BuildSetWindow(dpi int, a Area) []byte {
	b := make([]byte, len(setWindowTemplate))
	copy(b, setWindowTemplate)
	binary.BigEndian.PutUint16(b[swXRes:], uint16(dpi))
	binary.BigEndian.PutUint16(b[swYRes:], uint16(dpi))
	binary.BigEndian.PutUint32(b[swULX:], uint32(a.X))
	binary.BigEndian.PutUint32(b[swULY:], uint32(a.Y))
	binary.BigEndian.PutUint32(b[swWidth:], uint32(a.W))
	binary.BigEndian.PutUint32(b[swHeight:], uint32(a.H))
	return b
}

// GammaTable returns the 4096-byte gamma table for the WRITE(10) download.
// Without it the device returns a correctly sized image of all zeros, because
// the carriage never moves.
func GammaTable() []byte {
	g := make([]byte, GammaTableSize)
	n := copy(g, gammaHead)
	for i := n; i < len(g); i++ {
		v := 255 * math.Pow(float64(i)/float64(GammaTableSize-1), 1.0/1.8)
		g[i] = byte(math.Round(v))
	}
	return g
}

// planePad is the pixel boundary each colour plane is padded up to on the wire.
//
// Measured, not guessed: a 1666-pixel crop comes back with a 1680-pixel plane,
// which is a multiple of 16 but not of 32. Widths 637, 1275, 1460 and 2550 pad
// the same way under 16, 32 or 64, so they cannot tell those rules apart - do
// not "simplify" this to 32 on the strength of those.
const planePad = 16

// At 600 dpi, and only there, every plane carries one further block of
// planePad pixels beyond that rounding. Measured at six widths: 400 -> 416,
// 427 -> 448, 448 -> 464, 465 -> 496, 500 -> 528 and 1200 -> 1216, against
// 400 -> 400 and 427 -> 432 at 300 dpi and 427 -> 432 at 150. The extra block
// trails the image rather than leading it, so the pixels still start at column
// zero: a 600 dpi scan decoded without it comes out sheared into coloured
// stripes, which is what the padding rule is for.
const extraPadAbove = 300

// PlaneStride returns the padded width, in pixels, of one colour plane on the
// wire. The device pads each plane up to a multiple of planePad pixels - so a
// 1666-pixel line is sent as 1680 - and adds one more block above 300 dpi.
func PlaneStride(width, dpi int) int {
	stride := (width + planePad - 1) / planePad * planePad
	if dpi > extraPadAbove {
		stride += planePad
	}
	return stride
}

// WireSize returns how many bytes a scan of this pixel size occupies on the
// wire: three padded colour planes per line.
func WireSize(width, height, dpi int) int { return PlaneStride(width, dpi) * 3 * height }

// planeRowLag is how far each colour plane trails the red one, in wire lines.
//
// Measured, not guessed: the device hands all three planes of a line together,
// but green and blue do not describe the same row of the page as red, so a grey
// printed dither sampled through them comes out with a hue that rotates across
// the page, and horizontal edges get a colour fringe.
//
// From two captures of the same document, a table with rules and text at 300
// and at 600 dpi, aligning each plane's differenced row means against red's:
//
//	          300 dpi   600 dpi
//	green      0.15      0.18
//	blue       0.85      0.95
//
// The lag is in lines, not inches - a sensor with its three rows physically
// apart would double from 300 to 600 dpi, and this does not - so it is applied
// per line at every resolution rather than scaled.
var planeRowLag = [3]float64{0, 0.17, 0.90}

// planeLagWeight is planeRowLag as the 1/256 share of the previous wire line to
// mix into each plane, so the resample stays integer. The row splitter keeps
// one line of history, so every lag must be under one line; TestPlaneRowLag
// holds that invariant.
var planeLagWeight = func() [3]int {
	var w [3]int
	for i, lag := range planeRowLag {
		w[i] = int(math.Round(lag * 256))
	}
	return w
}()

// Rec. 601 luma weights over 256, which is what ModeGray averages the three
// planes with. They sum to exactly 256, so a neutral grey pixel keeps its
// value rather than drifting by a count.
const (
	lumaR = 77
	lumaG = 150
	lumaB = 29
)

// planeValue reads one pixel of one colour plane, resampled to red's row by
// mixing in that plane's share of the previous wire line.
func planeValue(cur, prev []byte, plane, c, x int) byte {
	w := planeLagWeight[c]
	v := cur[c*plane+x]
	if w == 0 {
		return v
	}
	return byte(((256-w)*int(v) + w*int(prev[c*plane+x]) + 128) >> 8)
}

// writeRow renders one output row from the three colour planes of wire line
// cur, at pixStride bytes per pixel: RGB for ModeColor, a single luma byte for
// ModeGray. prev is the wire line above, which planeValue resamples against;
// for the first line of an image, pass cur as prev - there is nothing above it.
func writeRow(dst []byte, pixStride int, mode Mode, cur, prev []byte, plane, width int) {
	if mode == ModeGray {
		for x := 0; x < width; x++ {
			r := int(planeValue(cur, prev, plane, 0, x))
			g := int(planeValue(cur, prev, plane, 1, x))
			b := int(planeValue(cur, prev, plane, 2, x))
			dst[x*pixStride] = byte((lumaR*r + lumaG*g + lumaB*b + 128) >> 8)
		}
		return
	}
	for c := 0; c < 3; c++ {
		for x := 0; x < width; x++ {
			dst[x*pixStride+c] = planeValue(cur, prev, plane, c, x)
		}
	}
}

// rowSplitter turns the device's planar wire format into interleaved RGB rows
// as the bytes arrive, so a scan can be streamed instead of buffered. Blocks
// from the device do not line up with lines on the wire, so whatever is left
// over after the last whole line is carried into the next block.
//
// The row it passes to emit is reused, so a consumer that keeps it must copy.
type rowSplitter struct {
	emit   func(y int, row []byte) error
	mode   Mode
	width  int // pixels per row
	pixel  int // bytes per pixel in the emitted row
	plane  int // padded pixels per colour plane
	stride int // bytes per wire line, all three planes
	held   []byte
	prev   []byte // previous whole wire line, for the planeRowLag resample
	row    []byte
	lines  int
}

func newRowSplitter(width, dpi int, mode Mode, emit func(y int, row []byte) error) *rowSplitter {
	plane, pixel := PlaneStride(width, dpi), mode.BytesPerPixel()
	return &rowSplitter{
		emit:   emit,
		mode:   mode,
		width:  width,
		pixel:  pixel,
		plane:  plane,
		stride: plane * 3,
		row:    make([]byte, width*pixel),
	}
}

func (w *rowSplitter) write(chunk []byte) error {
	w.held = append(w.held, chunk...)
	done := 0
	for len(w.held)-done >= w.stride {
		line := w.held[done : done+w.stride]
		above := line
		if w.lines > 0 {
			above = w.prev
		}
		writeRow(w.row, w.pixel, w.mode, line, above, w.plane, w.width)
		if err := w.emit(w.lines, w.row); err != nil {
			return err
		}
		w.prev = append(w.prev[:0], line...)
		done += w.stride
		w.lines++
	}
	w.held = append(w.held[:0], w.held[done:]...)
	return nil
}

// Deinterleave converts the device's planar output into an image. Each scan
// line arrives as three consecutive colour planes - the whole red row, then
// green, then blue - each padded to PlaneStride pixels; the padding columns are
// dropped here and each plane is resampled to red's row by planeRowLag. The
// resolution is needed because the padding depends on it, and the mode decides
// whether the result is RGB or the luma average of the three planes.
// Short input yields a correspondingly short image rather than an error, so a
// partial scan is still viewable.
func Deinterleave(raw []byte, width, height, dpi int, mode Mode) image.Image {
	plane := PlaneStride(width, dpi)
	stride := plane * 3
	lines := height
	if got := len(raw) / stride; got < lines {
		lines = got
	}
	rect := image.Rect(0, 0, width, lines)

	var pix []byte
	var pixStride, pixelBytes int
	var img image.Image
	if mode == ModeGray {
		g := image.NewGray(rect)
		img, pix, pixStride, pixelBytes = g, g.Pix, g.Stride, 1
	} else {
		c := image.NewRGBA(rect)
		img, pix, pixStride, pixelBytes = c, c.Pix, c.Stride, 4
	}

	for y := 0; y < lines; y++ {
		cur := raw[y*stride : y*stride+stride]
		above := cur
		if y > 0 {
			above = raw[(y-1)*stride : y*stride]
		}
		row := pix[y*pixStride:]
		writeRow(row, pixelBytes, mode, cur, above, plane, width)
		if pixelBytes == 4 {
			for x := 0; x < width; x++ {
				row[x*4+3] = 0xff
			}
		}
	}
	return img
}

// ToGray converts a finished image to the same 8-bit luma ModeGray produces.
// The Windows backend needs it: WIA hands over a colour bitmap and gives no
// say in the matter, so the conversion happens after the fact there.
func ToGray(src image.Image) *image.Gray {
	if g, ok := src.(*image.Gray); ok {
		return g
	}
	b := src.Bounds()
	dst := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, _ := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			dst.Pix[y*dst.Stride+x] = byte(
				(lumaR*int(r>>8) + lumaG*int(g>>8) + lumaB*int(bl>>8) + 128) >> 8)
		}
	}
	return dst
}
