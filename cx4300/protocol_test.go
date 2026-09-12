package cx4300

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"testing"
	"time"
)

// fakeScanner emulates the device's framing well enough to exercise the whole
// command sequence without hardware.
type fakeScanner struct {
	queue    [][]byte // frames waiting on bulk IN
	wantData int      // bytes still expected in a data-out phase
	wire     []byte   // synthetic image data
	wireAt   int
	cdbs     [][]byte // every CDB the device was sent
	latched  bool     // answer StatusDone to everything, like a poisoned device
}

func (f *fakeScanner) push(b ...byte)    { f.queue = append(f.queue, b) }
func (f *fakeScanner) pushBlob(b []byte) { f.queue = append(f.queue, b) }
func (f *fakeScanner) Close() error      { return nil }
func (f *fakeScanner) sentCDB(op byte) bool {
	for _, c := range f.cdbs {
		if len(c) > 0 && c[0] == op {
			return true
		}
	}
	return false
}

func (f *fakeScanner) BulkOut(data []byte, _ time.Duration) error {
	if f.wantData > 0 {
		f.wantData -= len(data)
		f.push(StatusDone, 0, 0, 0, 0, 0, 0, 0)
		return nil
	}
	cdb := append([]byte(nil), data...)
	f.cdbs = append(f.cdbs, cdb)

	if f.latched {
		f.push(StatusDone, 0, 0, 0, 0, 0, 0, 0)
		return nil
	}

	switch cdb[0] {
	case 0x12: // INQUIRY
		n := int(cdb[4])
		payload := make([]byte, n)
		copy(payload, []byte{0x06, 0, 0x02, 0x02, 0x49, 0, 0, 0})
		copy(payload[8:], []byte("Color   Color MFP01     0119"))
		if n > 40 {
			copy(payload[40:], []byte("Thu Oct 12 2006 10:12"))
		}
		f.push(StatusData, 0, 0, 0, 0, 0, 0, 0)
		f.pushBlob(payload)
		f.push(StatusDone, 0, 0, 0, 0, 0, 0, 0)
	case 0x24: // SET WINDOW: parameter length is a single byte
		f.wantData = int(cdb[8])
		f.push(StatusSend, 0, 0, 0, 0, 0, 0, 0)
	case 0x2a: // WRITE(10): 24-bit length
		f.wantData = int(cdb[6])<<16 | int(cdb[7])<<8 | int(cdb[8])
		f.push(StatusSend, 0, 0, 0, 0, 0, 0, 0)
	case 0x28: // READ
		want := int(cdb[6])<<16 | int(cdb[7])<<8 | int(cdb[8])
		if want == 0 {
			f.push(StatusDone, 0, 0, 0, 0, 0, 0, 0)
			break
		}
		payload := make([]byte, want)
		if cdb[2] == 0x00 { // image data
			n := copy(payload, f.wire[f.wireAt:])
			f.wireAt += n
			payload = payload[:n]
		}
		f.push(StatusData, 0, 0, 0, 0, 0, 0, 0)
		f.pushBlob(payload)
		f.push(StatusDone, 0, 0, 0, 0, 0, 0, 0)
	default: // TEST UNIT READY, RESERVE, RELEASE, SCAN
		f.push(StatusDone, 0, 0, 0, 0, 0, 0, 0)
	}
	return nil
}

func (f *fakeScanner) BulkIn(buf []byte, _ time.Duration) (int, error) {
	if len(f.queue) == 0 {
		return 0, errors.New("timeout")
	}
	// A real bulk read returns at most len(buf) bytes and leaves the rest of
	// the frame queued, so model that rather than dropping the remainder.
	frame := f.queue[0]
	n := copy(buf, frame)
	if n < len(frame) {
		f.queue[0] = frame[n:]
	} else {
		f.queue = f.queue[1:]
	}
	return n, nil
}

func TestBuildSetWindow(t *testing.T) {
	area := Area{X: 600, Y: 1200, W: 3000, H: 4200}
	b := BuildSetWindow(300, area)
	if len(b) != 58 {
		t.Fatalf("parameter block is %d bytes, want 58", len(b))
	}
	if got := binary.BigEndian.Uint16(b[swDescLen:]); got != 50 {
		t.Errorf("descriptor length %d, want 50", got)
	}
	if got := binary.BigEndian.Uint16(b[swXRes:]); got != 300 {
		t.Errorf("x resolution %d, want 300", got)
	}
	if got := binary.BigEndian.Uint16(b[swYRes:]); got != 300 {
		t.Errorf("y resolution %d, want 300", got)
	}
	for _, tc := range []struct {
		name string
		off  int
		want uint32
	}{
		{"ulx", swULX, 600}, {"uly", swULY, 1200},
		{"width", swWidth, 3000}, {"height", swHeight, 4200},
	} {
		if got := binary.BigEndian.Uint32(b[tc.off:]); got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, got, tc.want)
		}
	}
	// Fields we must not disturb: RGB composition and bit depth.
	if b[33] != 0x05 || b[34] != 0x08 {
		t.Errorf("image composition/depth = %02x %02x, want 05 08", b[33], b[34])
	}
}

func TestGammaTable(t *testing.T) {
	g := GammaTable()
	if len(g) != GammaTableSize {
		t.Fatalf("gamma table is %d bytes, want %d", len(g), GammaTableSize)
	}
	if !bytes.Equal(g[:len(gammaHead)], gammaHead) {
		t.Error("gamma table does not start with the captured bytes")
	}
	for i := 1; i < len(g); i++ {
		if g[i] < g[i-1] {
			t.Fatalf("gamma table is not monotonic at %d (%d < %d)", i, g[i], g[i-1])
		}
	}
	if g[len(g)-1] != 255 {
		t.Errorf("gamma table ends at %d, want 255", g[len(g)-1])
	}
}

func TestPlaneStride(t *testing.T) {
	// Every case below was measured against the device. 1666 is the important
	// one: it is the only width here that distinguishes padding to 16 from
	// padding to 32 or 64, and it is what caught the bug that a 32-pixel rule
	// shears cropped scans.
	for _, tc := range []struct{ in, want int }{
		{1275, 1280}, // 150 dpi full bed
		{637, 640},   // 75 dpi full bed
		{1460, 1472}, // 300 dpi crop
		{1666, 1680}, // 300 dpi crop; 32-rule would say 1696, 64-rule 1728
		{2550, 2560}, // 300 dpi full bed
		{1680, 1680}, // already aligned
	} {
		if got := PlaneStride(tc.in); got != tc.want {
			t.Errorf("PlaneStride(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestDeinterleave(t *testing.T) {
	const w, h = 5, 2
	plane := PlaneStride(w) // 32
	raw := make([]byte, plane*3*h)
	for y := 0; y < h; y++ {
		o := y * plane * 3
		for x := 0; x < w; x++ {
			raw[o+x] = byte(10 + x)          // red plane
			raw[o+plane+x] = byte(100 + x)   // green plane
			raw[o+2*plane+x] = byte(200 + y) // blue plane
		}
		// Padding columns carry junk that must not reach the image.
		for x := w; x < plane; x++ {
			raw[o+x], raw[o+plane+x], raw[o+2*plane+x] = 0xff, 0xff, 0xff
		}
	}
	img := Deinterleave(raw, w, h)
	if got := img.Bounds(); got != image.Rect(0, 0, w, h) {
		t.Fatalf("bounds %v, want %v", got, image.Rect(0, 0, w, h))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			wantR, wantG, wantB := uint32(10+x), uint32(100+x), uint32(200+y)
			if r>>8 != wantR || g>>8 != wantG || b>>8 != wantB || a>>8 != 0xff {
				t.Fatalf("pixel (%d,%d) = %d,%d,%d,%d want %d,%d,%d,255",
					x, y, r>>8, g>>8, b>>8, a>>8, wantR, wantG, wantB)
			}
		}
	}
}

func TestScanSequenceAndImage(t *testing.T) {
	const dpi = 75
	area := FullBed()
	w, h := area.Pixels(dpi)

	f := &fakeScanner{wire: make([]byte, WireSize(w, h))}
	plane := PlaneStride(w)
	for y := 0; y < h; y++ {
		o := y * plane * 3
		for x := 0; x < w; x++ {
			f.wire[o+x] = byte(x)
			f.wire[o+plane+x] = byte(y)
			f.wire[o+2*plane+x] = 0x40
		}
	}

	var lastDone, lastTotal int
	d := New(f)
	d.Progress = func(done, total int) { lastDone, lastTotal = done, total }

	img, err := d.Scan(Params{DPI: dpi, Area: area})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got, want := img.Bounds(), image.Rect(0, 0, w, h); got != want {
		t.Errorf("bounds %v, want %v", got, want)
	}
	r, g, b, _ := img.At(3, 4).RGBA()
	if r>>8 != 3 || g>>8 != 4 || b>>8 != 0x40 {
		t.Errorf("pixel (3,4) = %d,%d,%d want 3,4,64", r>>8, g>>8, b>>8)
	}
	if lastDone != lastTotal || lastTotal != WireSize(w, h) {
		t.Errorf("progress ended at %d/%d, want %d/%d", lastDone, lastTotal, WireSize(w, h), WireSize(w, h))
	}

	// The order-sensitive parts of the sequence must all have happened.
	for _, op := range []byte{0x12, 0x00, 0x16, 0x24, 0x2a, 0x1b, 0x28, 0x17} {
		if !f.sentCDB(op) {
			t.Errorf("command 0x%02x was never sent", op)
		}
	}
}

func TestScanReportsLatchedDevice(t *testing.T) {
	d := New(&fakeScanner{latched: true})
	_, err := d.Scan(Params{DPI: 75, Area: FullBed()})
	if !errors.Is(err, ErrLatched) {
		t.Fatalf("got %v, want ErrLatched", err)
	}
}

func TestParamsValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Params
		ok   bool
	}{
		{"full bed 150", Params{DPI: 150, Area: FullBed()}, true},
		{"bad dpi", Params{DPI: 200, Area: FullBed()}, false},
		{"empty area", Params{DPI: 150, Area: Area{}}, false},
		{"off the bed", Params{DPI: 150, Area: Area{X: 5000, Y: 0, W: 500, H: 500}}, false},
		{"sub-pixel", Params{DPI: 75, Area: Area{W: 3, H: 3}}, false},
	} {
		err := tc.p.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
