package main

import (
	"encoding/binary"
	"image"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

// row builds one full-resolution row of width pixels, all the same colour.
func row(width int, r, g, b byte) []byte {
	out := make([]byte, width*3)
	for x := 0; x < width; x++ {
		out[x*3+0], out[x*3+1], out[x*3+2] = r, g, b
	}
	return out
}

func TestLiveAveragesDownToDisplaySize(t *testing.T) {
	l := newLive()
	l.start(4, 4, 2) // 4x4 source, longest edge capped at 2: 2x2 blocks

	if l.w != 2 || l.h != 2 || l.box != 2 {
		t.Fatalf("display size %dx%d box %d, want 2x2 box 2", l.w, l.h, l.box)
	}
	// Two source rows of 100 and two of 200 average to one displayed row each.
	l.put(row(4, 100, 100, 100), 3)
	l.put(row(4, 100, 100, 100), 3)
	if l.rows != 1 {
		t.Fatalf("after two source rows the display has %d rows, want 1", l.rows)
	}
	l.put(row(4, 200, 200, 200), 3)
	l.put(row(4, 0, 0, 0), 3)
	l.finish()

	if l.rows != 2 {
		t.Fatalf("finished with %d display rows, want 2", l.rows)
	}
	if got := l.rgb[0]; got != 100 {
		t.Errorf("first display row = %d, want 100", got)
	}
	if got := l.rgb[l.w*3]; got != 100 {
		t.Errorf("second display row = %d, want the average of 200 and 0", got)
	}
}

func TestLiveFinishesAPartialBlock(t *testing.T) {
	l := newLive()
	l.start(3, 3, 2) // 2x2 blocks over a 3x3 image: the last row and column are short
	if l.w != 2 || l.h != 2 {
		t.Fatalf("display size %dx%d, want 2x2", l.w, l.h)
	}
	for i := 0; i < 3; i++ {
		l.put(row(3, 60, 60, 60), 3)
	}
	if l.rows != 1 {
		t.Fatalf("before finish %d rows are complete, want 1", l.rows)
	}
	l.finish()
	if l.rows != 2 {
		t.Fatalf("after finish %d rows, want the part-built one flushed too", l.rows)
	}
	// Every source pixel was 60, so every displayed pixel is 60 whatever the
	// block it was averaged over - this is what catches a wrong divisor at the
	// short edges.
	for i, v := range l.rgb {
		if v != 60 {
			t.Fatalf("display pixel %d = %d, want 60 (wrong divisor at an edge)", i, v)
		}
	}
}

func TestLiveUnscannedAreaIsBlank(t *testing.T) {
	l := newLive()
	l.start(2, 4, 0)
	if got := l.rgb[len(l.rgb)-1]; got != 0xff {
		t.Errorf("unscanned area is %d, want white (0xff)", got)
	}
}

func TestLiveReadWakesAndEndsWithTheScan(t *testing.T) {
	l := newLive()
	l.start(2, 2, 0)

	// Nothing yet: a reader is handed a channel to wait on.
	rows, done, wait, ok := l.read(l.gen, 0)
	if !ok || done || len(rows) != 0 || wait == nil {
		t.Fatalf("read on an empty scan = (%d bytes, done=%v, wait=%v, ok=%v)", len(rows), done, wait, ok)
	}
	l.put(row(2, 1, 2, 3), 3)
	select {
	case <-wait:
	default:
		t.Fatal("a completed row did not wake the reader")
	}
	rows, _, _, ok = l.read(l.gen, 0)
	if !ok || len(rows) != 2*3 {
		t.Fatalf("read returned %d bytes, want one 2-pixel row", len(rows))
	}

	// A second scan invalidates the first stream rather than mixing images.
	l.start(2, 2, 0)
	if _, _, _, ok := l.read(l.gen-1, 0); ok {
		t.Error("a stream from the previous scan was not ended")
	}
}

func TestHandleLiveStreamsBands(t *testing.T) {
	s := &server{live: newLive()}

	// No scan announced yet.
	rec := httptest.NewRecorder()
	s.handleLive(rec, httptest.NewRequest(http.MethodGet, "/api/live", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("with no scan running the endpoint answered %d, want 204", rec.Code)
	}

	s.live.start(2, 2, 0)
	s.live.put(row(2, 10, 20, 30), 3)
	s.live.put(row(2, 40, 50, 60), 3)
	s.live.finish()

	srv := httptest.NewServer(http.HandlerFunc(s.handleLive))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}

	if string(body[:4]) != liveMagic {
		t.Fatalf("stream starts with %q, want %q", body[:4], liveMagic)
	}
	w := binary.BigEndian.Uint32(body[8:])
	h := binary.BigEndian.Uint32(body[12:])
	if w != 2 || h != 2 {
		t.Fatalf("header says %dx%d, want 2x2", w, h)
	}
	rest := body[liveHeaderSize:]
	if first := binary.BigEndian.Uint32(rest[0:]); first != 0 {
		t.Errorf("first band starts at row %d, want 0", first)
	}
	count := binary.BigEndian.Uint32(rest[4:])
	if count != 2 {
		t.Errorf("first band holds %d rows, want both", count)
	}
	pixels := rest[8:]
	if len(pixels) != int(count)*int(w)*3 {
		t.Fatalf("band payload is %d bytes, want %d", len(pixels), int(count)*int(w)*3)
	}
	if pixels[0] != 10 || pixels[1] != 20 || pixels[2] != 30 {
		t.Errorf("first pixel = %d,%d,%d want 10,20,30", pixels[0], pixels[1], pixels[2])
	}
	if pixels[w*3] != 40 {
		t.Errorf("second row starts with %d, want 40", pixels[w*3])
	}
}

// Stopping a scan must not stop the transfer: this device locks up until its
// mains lead is pulled when one is abandoned part way through. So the rows keep
// being read - and keep being dropped.
func TestStoppedScanKeepsReadingAndKeepsNothing(t *testing.T) {
	const width, height = 4, 6
	stop := false
	fake := rowFeeder{width: width, height: height, onRow: func(y int) {
		if y == 2 {
			stop = true // the user presses Stop a third of the way in
		}
	}}

	l := newLive()
	l.start(width, height, 0)
	img, err := scanStreaming(&fake, cx4300.Params{DPI: 75, Area: cx4300.Area{
		X: 0, Y: 0, W: width * cx4300.Unit / 75, H: height * cx4300.Unit / 75,
	}}, l, func() bool { return stop })
	if err != nil {
		t.Fatalf("scanStreaming: %v", err)
	}
	if fake.delivered != height {
		t.Errorf("the device delivered %d of %d rows; a stopped scan must still be drained",
			fake.delivered, height)
	}
	if img == nil {
		t.Fatal("no image returned")
	}
	// Rows from after the stop were never copied in, so they are still zero -
	// which is what makes dropping the image the right thing for the caller.
	last := img.(interface{ At(x, y int) color.Color }).At(0, height-1)
	if _, _, _, a := last.RGBA(); a != 0 {
		t.Error("a row scanned after the stop was kept")
	}
}

// rowFeeder stands in for the device: it hands over every row, whatever the
// caller does with them.
type rowFeeder struct {
	width, height int
	delivered     int
	onRow         func(y int)
}

func (f *rowFeeder) ScanRows(p cx4300.Params, fn func(y int, row []byte) error) (int, int, error) {
	row := make([]byte, f.width*p.Mode.BytesPerPixel())
	for y := 0; y < f.height; y++ {
		if f.onRow != nil {
			f.onRow(y)
		}
		for i := range row {
			row[i] = byte(y + 1)
		}
		if err := fn(y, row); err != nil {
			return 0, 0, err
		}
		f.delivered++
	}
	return f.width, f.height, nil
}

// A grey row carries one byte per pixel; the live view is RGB whatever the
// mode, so it has to reach all three channels or the page draws a red image.
func TestLivePutGrayRowFillsAllChannels(t *testing.T) {
	l := newLive()
	l.start(2, 1, 0)
	l.put([]byte{40, 200}, 1)
	l.finish()

	rows, _, _, ok := l.read(l.gen, 0)
	if !ok || len(rows) != 2*3 {
		t.Fatalf("read returned %d bytes, ok=%v; want 6", len(rows), ok)
	}
	for x, want := range []byte{40, 200} {
		for c := 0; c < 3; c++ {
			if got := rows[x*3+c]; got != want {
				t.Errorf("pixel %d channel %d = %d, want %d", x, c, got, want)
			}
		}
	}
}

// A grey scan must be kept as an 8-bit grey image, so the PNG written to disk
// is a real greyscale file rather than three equal channels.
func TestScanStreamingGrayKeepsGrayImage(t *testing.T) {
	const width, height = 4, 3
	fake := rowFeeder{width: width, height: height}
	l := newLive()
	l.start(width, height, 0)
	img, err := scanStreaming(&fake, cx4300.Params{
		DPI:  75,
		Area: cx4300.Area{X: 0, Y: 0, W: width * cx4300.Unit / 75, H: height * cx4300.Unit / 75},
		Mode: cx4300.ModeGray,
	}, l, func() bool { return false })
	if err != nil {
		t.Fatalf("scanStreaming: %v", err)
	}
	gray, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("a grey scan produced %T, want *image.Gray", img)
	}
	// rowFeeder fills row y with y+1.
	for y := 0; y < height; y++ {
		if got := gray.GrayAt(0, y).Y; got != byte(y+1) {
			t.Errorf("row %d = %d, want %d", y, got, y+1)
		}
	}
}

func TestRequestModeFallsBackToServerDefault(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		name   string
		server bool
		req    *bool
		want   cx4300.Mode
	}{
		{"server colour, request silent", false, nil, cx4300.ModeColor},
		{"server grey, request silent", true, nil, cx4300.ModeGray},
		{"server colour, request asks grey", false, &yes, cx4300.ModeGray},
		{"server grey, request asks colour", true, &no, cx4300.ModeColor},
	} {
		s := &server{gray: c.server}
		if got := s.mode(scanRequest{Gray: c.req}); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
