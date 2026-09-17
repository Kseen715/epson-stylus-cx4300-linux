package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"net/http"
	"sync"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

// The scan in progress, at the size the browsers display, so a page can watch
// the image appear row by row instead of staring at a progress bar for the two
// minutes a 300 dpi full bed takes.
//
// Only the shrunk copy is shared: a 600 dpi full bed is 107 MB, which is worth
// neither the network nor the canvas on a phone, and the finished scan is saved
// to disk at full resolution regardless. Rows are averaged down by whole
// integer blocks - the same box average the finished image gets from shrink -
// so what appears during the scan matches what replaces it at the end.
//
// The wire format is deliberately trivial, because a browser has to parse it:
// a 16-byte header, then bands of rows as they are completed.
//
//	header: "ESCL" | uint32 generation | uint32 width | uint32 height
//	band:   uint32 first row | uint32 row count | rows, 3 bytes per pixel
//
// All integers are big-endian, which is what DataView reads by default.
const liveMagic = "ESCL"

const liveHeaderSize = 16

type live struct {
	mu   sync.Mutex
	gen  int  // bumped per scan, so a stale stream ends rather than mixing scans
	on   bool // a scan has been announced
	done bool
	w, h int
	box  int    // source pixels per displayed pixel, both axes
	rgb  []byte // w*h*3, white where nothing has been scanned yet
	rows int    // displayed rows completed

	// The displayed row being accumulated, and how many source rows and
	// columns fold into it. The last row and column of the image may be built
	// from fewer than box of them, so the divisor is counted, not assumed.
	acc  []uint32
	accN int
	cols []uint32

	// ch is closed on every change and replaced, which is how a reader waits
	// for the next rows without polling and without missing a wake-up.
	ch chan struct{}
}

func newLive() *live { return &live{ch: make(chan struct{})} }

// start announces a new scan of the given full-resolution size. It is called
// when the scan is accepted rather than when the device answers, so a browser
// that asks for the stream immediately finds it there.
func (l *live) start(fullW, fullH, maxEdge int) {
	box := 1
	if maxEdge > 0 {
		longest := fullW
		if fullH > longest {
			longest = fullH
		}
		if longest > maxEdge {
			box = (longest + maxEdge - 1) / maxEdge
		}
	}
	w := (fullW + box - 1) / box
	h := (fullH + box - 1) / box

	l.mu.Lock()
	defer l.mu.Unlock()
	l.gen++
	l.on, l.done = true, false
	l.w, l.h, l.box = w, h, box
	l.rgb = make([]byte, w*h*3)
	for i := range l.rgb {
		l.rgb[i] = 0xff // unscanned area reads as blank paper, not black
	}
	l.rows = 0
	l.acc = make([]uint32, w*3)
	l.accN = 0
	l.cols = make([]uint32, w)
	for x := 0; x < fullW; x++ {
		l.cols[x/box]++
	}
	l.notify()
}

// put folds one full-resolution row of RGB triples into the displayed image,
// completing a displayed row every box rows.
func (l *live) put(row []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.on || l.done {
		return
	}
	for x := 0; x*3+2 < len(row) && x/l.box < l.w; x++ {
		o := (x / l.box) * 3
		l.acc[o+0] += uint32(row[x*3+0])
		l.acc[o+1] += uint32(row[x*3+1])
		l.acc[o+2] += uint32(row[x*3+2])
	}
	l.accN++
	if l.accN >= l.box {
		l.flush()
	}
}

// finish ends the scan, emitting a part-built row if the image did not divide
// evenly, and wakes every reader so their streams end.
func (l *live) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.on || l.done {
		return
	}
	l.flush()
	l.done = true
	l.notify()
}

// flush writes the accumulated row into the image. The caller holds mu.
func (l *live) flush() {
	if l.accN == 0 || l.rows >= l.h {
		return
	}
	o := l.rows * l.w * 3
	for x := 0; x < l.w; x++ {
		n := uint32(l.accN) * l.cols[x]
		if n == 0 {
			n = 1
		}
		l.rgb[o+x*3+0] = byte(l.acc[x*3+0] / n)
		l.rgb[o+x*3+1] = byte(l.acc[x*3+1] / n)
		l.rgb[o+x*3+2] = byte(l.acc[x*3+2] / n)
	}
	l.rows++
	for i := range l.acc {
		l.acc[i] = 0
	}
	l.accN = 0
	l.notify()
}

// notify wakes everyone waiting and arms the next wait. The caller holds mu.
func (l *live) notify() {
	close(l.ch)
	l.ch = make(chan struct{})
}

// begun reports the scan a stream should follow.
func (l *live) begun() (gen, w, h int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gen, l.w, l.h, l.on
}

// read hands back whatever has been completed past have. When there is nothing
// new it returns a channel that closes as soon as there is. ok is false once
// another scan has taken over, which ends the stream rather than mixing two
// images together.
func (l *live) read(gen, have int) (rows []byte, done bool, wait <-chan struct{}, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gen != gen {
		return nil, true, nil, false
	}
	if l.rows > have {
		return append([]byte(nil), l.rgb[have*l.w*3:l.rows*l.w*3]...), l.done, nil, true
	}
	return nil, l.done, l.ch, true
}

// handleLive streams the scan in progress. It answers 204 when no scan has been
// started, so a page that asks at the wrong moment simply does not draw.
func (s *server) handleLive(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	gen, width, height, on := s.live.begun()
	if !on {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	head := w.Header()
	head.Set("Content-Type", "application/octet-stream")
	head.Set("Cache-Control", "no-store")
	// A buffering reverse proxy would hold the whole scan back to the end,
	// which is precisely what this endpoint exists to avoid.
	head.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	hdr := make([]byte, liveHeaderSize)
	copy(hdr, liveMagic)
	binary.BigEndian.PutUint32(hdr[4:], uint32(gen))
	binary.BigEndian.PutUint32(hdr[8:], uint32(width))
	binary.BigEndian.PutUint32(hdr[12:], uint32(height))
	if _, err := w.Write(hdr); err != nil {
		return
	}
	flusher.Flush()

	band := make([]byte, 8)
	sent := 0
	for {
		rows, done, wait, ok := s.live.read(gen, sent)
		if !ok {
			return
		}
		if len(rows) > 0 {
			count := len(rows) / (width * 3)
			binary.BigEndian.PutUint32(band[0:], uint32(sent))
			binary.BigEndian.PutUint32(band[4:], uint32(count))
			if _, err := w.Write(band); err != nil {
				return
			}
			if _, err := w.Write(rows); err != nil {
				return
			}
			flusher.Flush()
			sent += count
			continue
		}
		if done {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-wait:
		}
	}
}

// scanStreaming runs a scan that arrives row by row, building the full
// resolution image for saving while feeding the shrunk copy the browsers watch.
//
// stopped reports that the user has asked for the scan to end. The rows are
// then dropped rather than kept, but they are still read: this device locks up
// until its mains lead is pulled if a transfer is abandoned part way through,
// so the only safe way to stop early is to stop caring about what arrives.
func scanStreaming(sc cx4300.StreamScanner, p cx4300.Params, l *live, stopped func() bool) (image.Image, error) {
	width, height := p.Area.Pixels(p.DPI)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	_, rows, err := sc.ScanRows(p, func(y int, row []byte) error {
		if stopped() {
			return nil
		}
		if y < height {
			o := y * img.Stride
			for x := 0; x < width; x++ {
				img.Pix[o+x*4+0] = row[x*3+0]
				img.Pix[o+x*4+1] = row[x*3+1]
				img.Pix[o+x*4+2] = row[x*3+2]
				img.Pix[o+x*4+3] = 0xff
			}
		}
		l.put(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if rows <= 0 {
		return nil, fmt.Errorf("the scan produced no rows")
	}
	if rows < height {
		// The device ended the image early; keep what it did send.
		return img.SubImage(image.Rect(0, 0, width, rows)), nil
	}
	return img, nil
}
