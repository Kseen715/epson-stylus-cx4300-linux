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
	// Every case below was measured against the device by dumping the raw wire
	// data and recovering its line period. 1666 is the important one at 300
	// dpi: it is the only width there that distinguishes padding to 16 from
	// padding to 32 or 64, and it is what caught the bug that a 32-pixel rule
	// shears cropped scans. The 600 dpi rows are the second measurement: that
	// resolution, and only that one, adds a further 16-pixel block.
	for _, tc := range []struct{ width, dpi, want int }{
		{637, 75, 640},    // full bed
		{427, 150, 432},   //
		{1275, 150, 1280}, // full bed
		{400, 300, 400},   // already aligned, and no extra block below 600 dpi
		{427, 300, 432},   //
		{1460, 300, 1472}, //
		{1666, 300, 1680}, // 32-rule would say 1696, 64-rule 1728
		{2550, 300, 2560}, // full bed
		{400, 600, 416},   // aligned, yet still one block more
		{427, 600, 448},   // the width from the mangled 600 dpi scan
		{448, 600, 464},   //
		{465, 600, 496},   // 496 is not a multiple of 32, which rules that out
		{500, 600, 528},   //
		{1200, 600, 1216}, // the extra block is not a small-width effect
	} {
		if got := PlaneStride(tc.width, tc.dpi); got != tc.want {
			t.Errorf("PlaneStride(%d, %d dpi) = %d, want %d", tc.width, tc.dpi, got, tc.want)
		}
	}
}

func TestDeinterleave(t *testing.T) {
	const w, h = 5, 2
	plane := PlaneStride(w, 300) // 32
	raw := make([]byte, plane*3*h)
	for y := 0; y < h; y++ {
		o := y * plane * 3
		for x := 0; x < w; x++ {
			raw[o+x] = byte(10 + x)          // red plane
			raw[o+plane+x] = byte(100 + x)   // green plane
			raw[o+2*plane+x] = byte(200 + x) // blue plane
		}
		// Padding columns carry junk that must not reach the image.
		for x := w; x < plane; x++ {
			raw[o+x], raw[o+plane+x], raw[o+2*plane+x] = 0xff, 0xff, 0xff
		}
	}
	img := Deinterleave(raw, w, h, 300, ModeColor)
	if got := img.Bounds(); got != image.Rect(0, 0, w, h) {
		t.Fatalf("bounds %v, want %v", got, image.Rect(0, 0, w, h))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			wantR, wantG, wantB := uint32(10+x), uint32(100+x), uint32(200+x)
			if r>>8 != wantR || g>>8 != wantG || b>>8 != wantB || a>>8 != 0xff {
				t.Fatalf("pixel (%d,%d) = %d,%d,%d,%d want %d,%d,%d,255",
					x, y, r>>8, g>>8, b>>8, a>>8, wantR, wantG, wantB)
			}
		}
	}
}

// The row splitter carries one line of history, so a plane cannot be allowed to
// trail by a whole line or more.
func TestPlaneRowLag(t *testing.T) {
	if planeRowLag[0] != 0 {
		t.Errorf("red is the reference plane, its lag must be 0, got %v", planeRowLag[0])
	}
	for i, lag := range planeRowLag {
		if lag < 0 || lag >= 1 {
			t.Errorf("plane %d lag %v is outside [0,1) - the row splitter keeps only one line", i, lag)
		}
	}
}

// A page whose brightness ramps down the image, sampled by planes that trail
// red by planeRowLag, must decode back to three channels that agree: that is
// what the resample is for. The first row is excluded because it has no line
// above it to mix.
func TestDeinterleaveCorrectsPlaneLag(t *testing.T) {
	const w, h, dpi = 4, 12, 300
	plane := PlaneStride(w, dpi)
	content := func(y float64) byte { return byte(20 + 10*y) }
	raw := make([]byte, plane*3*h)
	for y := 0; y < h; y++ {
		for c := 0; c < 3; c++ {
			for x := 0; x < w; x++ {
				raw[y*plane*3+c*plane+x] = content(float64(y) + planeRowLag[c])
			}
		}
	}
	img := Deinterleave(raw, w, h, dpi, ModeColor)
	for y := 1; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			want := uint32(content(float64(y)))
			for i, got := range []uint32{r >> 8, g >> 8, b >> 8} {
				if diff := int(got) - int(want); diff < -1 || diff > 1 {
					t.Fatalf("pixel (%d,%d) channel %d = %d, want %d: plane lag not corrected",
						x, y, i, got, want)
				}
			}
		}
	}
}

// ModeGray must deliver the luma average of the three planes, one byte per
// pixel, with the same plane lag correction colour gets.
func TestDeinterleaveGray(t *testing.T) {
	const w, h, dpi = 3, 4, 300
	plane := PlaneStride(w, dpi)
	raw := make([]byte, plane*3*h)
	for y := 0; y < h; y++ {
		for x := 0; x < plane; x++ {
			// Constant down the page, so the lag resample is a no-op and the
			// expected luma can be written out by hand.
			raw[y*plane*3+x] = 90         // red
			raw[y*plane*3+plane+x] = 160  // green
			raw[y*plane*3+2*plane+x] = 30 // blue
		}
	}
	img := Deinterleave(raw, w, h, dpi, ModeGray)
	gray, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("ModeGray returned %T, want *image.Gray", img)
	}
	if got := gray.Bounds(); got != image.Rect(0, 0, w, h) {
		t.Fatalf("bounds %v, want %v", got, image.Rect(0, 0, w, h))
	}
	want := byte((lumaR*90 + lumaG*160 + lumaB*30 + 128) >> 8)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if got := gray.GrayAt(x, y).Y; got != want {
				t.Fatalf("pixel (%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
}

// A neutral grey original must come back with its value intact: the weights sum
// to 256 exactly so that grey does not drift, and ToGray must agree with the
// decode path rather than being a second, subtly different formula.
func TestGrayIsNeutralAndToGrayAgrees(t *testing.T) {
	const w, h, dpi = 4, 3, 300
	plane := PlaneStride(w, dpi)
	for _, level := range []byte{0, 17, 128, 200, 255} {
		raw := make([]byte, plane*3*h)
		for i := range raw {
			raw[i] = level
		}
		gray := Deinterleave(raw, w, h, dpi, ModeGray).(*image.Gray)
		if got := gray.GrayAt(1, 1).Y; got != level {
			t.Errorf("neutral %d decoded as %d", level, got)
		}
		colour := Deinterleave(raw, w, h, dpi, ModeColor)
		if got := ToGray(colour).GrayAt(1, 1).Y; got != level {
			t.Errorf("neutral %d through ToGray became %d", level, got)
		}
	}
}

func TestParamsValidateRejectsUnknownMode(t *testing.T) {
	p := Params{DPI: 300, Area: FullBed(), Mode: Mode(7)}
	if err := p.Validate(); err == nil {
		t.Fatal("an unknown mode must be rejected")
	}
}

func TestScanSequenceAndImage(t *testing.T) {
	const dpi = 75
	area := FullBed()
	w, h := area.Pixels(dpi)

	f := &fakeScanner{wire: make([]byte, WireSize(w, h, dpi))}
	plane := PlaneStride(w, dpi)
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
	if lastDone != lastTotal || lastTotal != WireSize(w, h, dpi) {
		t.Errorf("progress ended at %d/%d, want %d/%d", lastDone, lastTotal,
			WireSize(w, h, dpi), WireSize(w, h, dpi))
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

// ScanRows streams the same image Scan builds in memory, so a SANE frontend
// reading rows as they arrive and the web UI holding the whole scan see
// identical pixels. The interesting case is a width whose plane is padded -
// 1666 pixels go on the wire as 1680 - since the padding columns have to be
// dropped while the data is still arriving in blocks that do not line up with
// rows.
func TestScanRowsMatchesScan(t *testing.T) {
	const dpi = 300
	area := Area{X: 0, Y: 0, W: 1666 * Unit / dpi, H: 40 * Unit / dpi}
	w, h := area.Pixels(dpi)
	if w != 1666 {
		t.Fatalf("test needs a 1666 pixel wide area, got %d", w)
	}

	wire := make([]byte, WireSize(w, h, dpi))
	for i := range wire {
		wire[i] = byte(i * 7)
	}
	p := Params{DPI: dpi, Area: area}

	img, err := New(&fakeScanner{wire: append([]byte(nil), wire...)}).Scan(p)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	var buf bytes.Buffer
	gotW, gotH, err := New(&fakeScanner{wire: append([]byte(nil), wire...)}).
		ScanRows(p, func(_ int, row []byte) error {
			_, err := buf.Write(row)
			return err
		})
	if err != nil {
		t.Fatalf("ScanRows: %v", err)
	}
	if gotW != w || gotH != h {
		t.Fatalf("ScanRows reported %dx%d, want %dx%d", gotW, gotH, w, h)
	}
	if buf.Len() != w*h*3 {
		t.Fatalf("ScanRows wrote %d bytes, want %d", buf.Len(), w*h*3)
	}

	rows := buf.Bytes()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			o := (y*w + x) * 3
			if byte(r>>8) != rows[o] || byte(g>>8) != rows[o+1] || byte(b>>8) != rows[o+2] {
				t.Fatalf("pixel (%d,%d): streamed %d,%d,%d but Scan has %d,%d,%d",
					x, y, rows[o], rows[o+1], rows[o+2], r>>8, g>>8, b>>8)
			}
		}
	}
}
