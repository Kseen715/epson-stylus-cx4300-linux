//go:build linux

package main

import (
	"errors"
	"io"
	"sync"
	"testing"
	"unsafe"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

func TestOptionTableDescribesTheDevice(t *testing.T) {
	opts := buildOptions()
	if len(opts) != numOptions {
		t.Fatalf("built %d options, want %d", len(opts), numOptions)
	}
	res := opts[optResolution]
	if res._type != typeInt || res.unit != unitDPI {
		t.Errorf("resolution is type %d unit %d", res._type, res.unit)
	}
	list := *(**saneWord)(unsafe.Pointer(&res.constraint[0]))
	got := unsafe.Slice(list, len(cx4300.SupportedDPI)+1)
	if int(got[0]) != len(cx4300.SupportedDPI) {
		t.Fatalf("word list says %d entries, want %d", got[0], len(cx4300.SupportedDPI))
	}
	for i, dpi := range cx4300.SupportedDPI {
		if int(got[i+1]) != dpi {
			t.Errorf("word list[%d] = %d, want %d", i, got[i+1], dpi)
		}
	}
	rng := *(**saneRange)(unsafe.Pointer(&opts[optBRY].constraint[0]))
	if want := saneWord(fix(bedHeightMM)); rng.max != want {
		t.Errorf("br-y range max %d, want %d (the full platen)", rng.max, want)
	}
}

func TestDefaultParamsAreTheWholePlaten(t *testing.T) {
	h := newHandle()
	p, err := h.params()
	if err != nil {
		t.Fatalf("default parameters are invalid: %v", err)
	}
	if p.Area != cx4300.FullBed() {
		t.Errorf("default area %+v, want the full bed %+v", p.Area, cx4300.FullBed())
	}
}

func TestSetGeometryPushesTheOtherEdge(t *testing.T) {
	h := newHandle()
	// A frontend that sets the top-left corner past the current bottom-right
	// one must not leave a negative area behind.
	h.setGeometry(optTLX, fix(200))
	if h.brx < h.tlx {
		t.Errorf("tl-x %v left br-x behind at %v", unfix(h.tlx), unfix(h.brx))
	}
	h.setGeometry(optBRY, fix(10))
	if h.tly > h.bry {
		t.Errorf("br-y %v left tl-y above it at %v", unfix(h.bry), unfix(h.tly))
	}
	if got := h.setGeometry(optBRX, fix(1000)); got != fix(bedWidthMM) {
		t.Errorf("br-x beyond the glass stored %v, want the bed width %v",
			unfix(got), bedWidthMM)
	}
	if got := h.setGeometry(optTLY, fix(-5)); got != 0 {
		t.Errorf("negative tl-y stored %v, want 0", unfix(got))
	}
}

func TestControlOptionRoundTrip(t *testing.T) {
	h := newHandle()
	var info saneInt

	var count saneInt
	if st := h.controlOption(optCount, actionGet, unsafe.Pointer(&count), nil); st != statusGood {
		t.Fatalf("reading the option count: status %d", st)
	}
	if count != numOptions {
		t.Errorf("option count %d, want %d", count, numOptions)
	}

	// An unsupported resolution snaps to a supported one and says so.
	dpi := saneInt(200)
	if st := h.controlOption(optResolution, actionSet, unsafe.Pointer(&dpi), &info); st != statusGood {
		t.Fatalf("setting the resolution: status %d", st)
	}
	if dpi != 150 {
		t.Errorf("200 dpi snapped to %d, want the nearest supported 150", dpi)
	}
	if info&infoInexact == 0 {
		t.Error("a snapped resolution did not report SANE_INFO_INEXACT")
	}
	dpi = 0
	if st := h.controlOption(optResolution, actionGet, unsafe.Pointer(&dpi), nil); st != statusGood {
		t.Fatalf("reading the resolution back: status %d", st)
	}
	if dpi != 150 {
		t.Errorf("read back %d dpi, want 150", dpi)
	}

	// Groups hold no value.
	if st := h.controlOption(optModeGroup, actionGet, unsafe.Pointer(&dpi), nil); st == statusGood {
		t.Error("reading a group's value succeeded; it has none")
	}
}

func TestGeometryToScanArea(t *testing.T) {
	h := newHandle()
	h.setGeometry(optTLX, fix(25.4)) // one inch in
	h.setGeometry(optBRX, fix(50.8)) // two inches in
	p, err := h.params()
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	if p.Area.X != cx4300.Unit || p.Area.W != cx4300.Unit {
		t.Errorf("1in..2in became X=%d W=%d, want %d and %d",
			p.Area.X, p.Area.W, cx4300.Unit, cx4300.Unit)
	}
}

func TestStreamDeliversThenEnds(t *testing.T) {
	s := newStream()
	go func() {
		s.Write([]byte("abcdef"))
		s.finish(nil)
	}()
	buf := make([]byte, 4)
	n, err := s.read(buf)
	if err != nil || n == 0 {
		t.Fatalf("read returned %d, %v", n, err)
	}
	rest := make([]byte, 16)
	total := n
	for {
		n, err = s.read(rest)
		total += n
		if err != nil {
			break
		}
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("stream ended with %v, want io.EOF", err)
	}
	if total != 6 {
		t.Errorf("read %d bytes, want 6", total)
	}
}

func TestStreamDiscardUnblocksTheScan(t *testing.T) {
	s := newStream()
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		// More than the high-water mark, so this blocks until discard lets go.
		// That is the point: a cancelled scan must still run to its end,
		// because this device wedges if it is abandoned mid-transfer.
		for i := 0; i < 8; i++ {
			s.Write(make([]byte, highWater/2))
		}
		s.finish(nil)
	}()
	s.discard()
	done.Wait()
	s.wait()
	if n, err := s.read(make([]byte, 16)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("a discarded stream returned %d bytes, %v", n, err)
	}
}

func TestStreamReportsTheScanError(t *testing.T) {
	s := newStream()
	s.finish(cx4300.ErrLatched)
	if _, err := s.read(make([]byte, 16)); !errors.Is(err, cx4300.ErrLatched) {
		t.Errorf("read returned %v, want the scan's own error", err)
	}
}
