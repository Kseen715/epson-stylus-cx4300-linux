//go:build linux

// Command libsane-cx4300 is a SANE backend for the Epson Stylus CX4300 family,
// built as a shared library that SANE's dll backend loads like any other
// backend. With it installed, XSane, GIMP, simple-scan, scanimage and anything
// else built on SANE can scan on this device, which none of them can otherwise
// do: the stock Epson backends speak ESC/I, which this device does not
// implement and which latches it into refusing every command until its mains
// power is cut.
//
// Build it with:
//
//	go build -buildmode=c-shared -o libsane-cx4300.so.1 ./cmd/libsane-cx4300
//
// then drop the result next to the other backends (/usr/lib/sane or
// /usr/lib/<triplet>/sane) and add a line "cx4300" to /etc/sane.d/dll.conf, or
// a file containing it to /etc/sane.d/dll.d/. install.sh does all of that.
//
// The backend holds the USB device only while a scan is actually running -
// from sane_start until the last row is read - so the escan web UI and a SANE
// frontend can be open at the same time; whichever one starts a scan first
// gets the device, and the other is told it is busy.
package main

/*
#include <stdlib.h>
#include <string.h>
#include "sane_abi.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

func main() {}

// The version of the SANE API this backend implements: 1.0, build 0.
const saneVersion = 1<<24 | 0<<16 | 0

// The platen in millimetres, which is the unit SANE geometry options use.
const (
	bedWidthMM  = float64(cx4300.BedWidth) / cx4300.Unit * 25.4
	bedHeightMM = float64(cx4300.BedHeight) / cx4300.Unit * 25.4
)

// calibrationPath is where this scanner's measured colour plane alignment is
// read from - the same file "escan calibrate" writes, so a unit calibrated once
// is calibrated for both frontends. A backend has no command line to override
// it with, hence the environment variable.
func calibrationPath() string {
	if p := os.Getenv("CX4300_CALIBRATION"); p != "" {
		return p
	}
	return defaultCalibrationPath
}

// deviceName is the name this backend answers to. SANE prefixes it with the
// backend name, so frontends show it as "cx4300:cx4300"; sane_open also accepts
// an empty name, which is what "scanimage" uses with no device given.
const deviceName = "cx4300"

// state, guarded by mu, is everything the backend owns. SANE makes no promises
// about which thread calls in, so every entry point takes this lock - except
// while sane_read waits for data, which would otherwise block sane_cancel.
var (
	mu      sync.Mutex
	inited  bool
	opts    []C.SANE_Option_Descriptor
	devices **C.SANE_Device // one device, NULL-terminated
	noDevs  **C.SANE_Device // NULL-terminated and empty
	handles = map[unsafe.Pointer]*handle{}
)

// handle is one open scanner. The option values live here, so two frontends
// with the device open at once do not disturb each other's settings.
type handle struct {
	dpi                int
	mode               cx4300.Mode
	oversample         bool
	tlx, tly, brx, bry C.SANE_Fixed // millimetres, 16.16 fixed point

	// Set between sane_start and the end of the scan.
	scanning  bool
	cancelled bool
	stream    *stream
	width     int
	total     int // bytes the frontend was promised
	sent      int // bytes handed to it so far
}

func newHandle() *handle {
	h := &handle{}
	h.setDefaults()
	return h
}

func (h *handle) setDefaults() {
	h.dpi = 300
	h.mode = cx4300.ModeColor
	// On by default: below 300 dpi the colour planes alias fine detail
	// differently and thin lines come out fringed. A frontend that would rather
	// have the shorter sweep turns it off.
	h.oversample = true
	h.tlx, h.tly = 0, 0
	h.brx, h.bry = fix(bedWidthMM), fix(bedHeightMM)
}

// params turns the option values into a scan request, rounding the millimetre
// geometry to the 1/600 inch grid the device works in.
func (h *handle) params() (cx4300.Params, error) {
	x0, y0 := mmToUnits(h.tlx), mmToUnits(h.tly)
	x1, y1 := mmToUnits(h.brx), mmToUnits(h.bry)
	p := cx4300.Params{
		DPI:        h.dpi,
		Area:       cx4300.Area{X: x0, Y: y0, W: x1 - x0, H: y1 - y0},
		Mode:       h.mode,
		Oversample: h.oversample,
	}
	return p, p.Validate()
}

func fix(mm float64) C.SANE_Fixed { return C.SANE_Fixed(math.Round(mm * (1 << 16))) }

func unfix(f C.SANE_Fixed) float64 { return float64(f) / (1 << 16) }

// mmToUnits converts a fixed-point millimetre value to the device's 1/600 inch
// units, clamped to the platen so a rounding error cannot ask for a scan that
// runs off the glass.
func mmToUnits(f C.SANE_Fixed) int {
	u := int(math.Round(unfix(f) / 25.4 * cx4300.Unit))
	return clamp(u, 0, cx4300.BedHeight)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// warn reports why something failed. SANE returns a status code and nothing
// else, so without this the operator of a GUI frontend has no way to learn that
// the scanner needs a power cycle or that the web UI is holding it.
func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cx4300: "+format+"\n", args...)
}

// statusFor maps a Go error onto the closest SANE status, so that frontends
// show "device busy" and "access denied" rather than a blanket I/O error.
func statusFor(err error) C.SANE_Status {
	switch {
	case err == nil:
		return C.SANE_STATUS_GOOD
	case errors.Is(err, syscall.EBUSY):
		return C.SANE_STATUS_DEVICE_BUSY
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES):
		return C.SANE_STATUS_ACCESS_DENIED
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENODEV):
		return C.SANE_STATUS_IO_ERROR
	default:
		return C.SANE_STATUS_IO_ERROR
	}
}

func lookup(sh C.SANE_Handle) *handle { return handles[unsafe.Pointer(sh)] }

//export sane_cx4300_init
func sane_cx4300_init(versionCode *C.SANE_Int, authorize C.SANE_Auth_Callback) C.SANE_Status {
	mu.Lock()
	defer mu.Unlock()
	if !inited {
		opts = buildOptions()
		devices, noDevs = buildDeviceLists()
		loadCalibration()
		inited = true
	}
	if versionCode != nil {
		*versionCode = saneVersion
	}
	return C.SANE_STATUS_GOOD
}

// loadCalibration puts this unit's measured plane alignment into force, and
// says so on stderr either way: a frontend shows no such thing, and silently
// decoding with the wrong unit's numbers is the failure this exists to prevent.
// The caller holds mu.
func loadCalibration() {
	path := calibrationPath()
	cal, found, err := cx4300.LoadCalibration(path)
	if err != nil {
		warn("%v", err)
		warn("using the built-in colour plane alignment instead")
	}
	if err := cx4300.UseCalibration(cal); err != nil {
		warn("calibration: %v", err) // the built-in one stays in force
		return
	}
	if found {
		warn("colour plane alignment from %s: green %.2f, blue %.2f",
			path, cal.PlaneRowLag[1], cal.PlaneRowLag[2])
	}
}

//export sane_cx4300_exit
func sane_cx4300_exit() {
	mu.Lock()
	open := make([]*handle, 0, len(handles))
	for p, h := range handles {
		open = append(open, h)
		delete(handles, p)
		C.free(p)
	}
	mu.Unlock()
	// Outside the lock: cancelling waits on the stream, which the scan
	// goroutine needs the lock-free path to finish.
	for _, h := range open {
		h.cancel()
	}
}

//export sane_cx4300_get_devices
func sane_cx4300_get_devices(list ***C.SANE_Device, localOnly C.SANE_Bool) C.SANE_Status {
	mu.Lock()
	defer mu.Unlock()
	if list == nil {
		return C.SANE_STATUS_INVAL
	}
	// Find only reads sysfs. Deliberately no USB traffic here: this runs for
	// every "scanimage -L", and the device must not be spoken to unless a scan
	// is actually wanted.
	found, err := cx4300.Find()
	if err != nil {
		*list = noDevs
		return C.SANE_STATUS_GOOD
	}
	if found.BehindHub {
		warn("%s is plugged in behind a USB hub; this scanner only works "+
			"on a root-hub port and will report an empty scan area", found.SysName)
	}
	*list = devices
	return C.SANE_STATUS_GOOD
}

//export sane_cx4300_open
func sane_cx4300_open(name C.SANE_String_Const, out *C.SANE_Handle) C.SANE_Status {
	mu.Lock()
	defer mu.Unlock()
	if out == nil {
		return C.SANE_STATUS_INVAL
	}
	if n := C.GoString(name); n != "" && n != deviceName {
		return C.SANE_STATUS_INVAL
	}
	// The device is not claimed until sane_start, but check now that it is
	// there at all, so a frontend fails on open rather than half way into a
	// scan dialogue.
	if _, err := cx4300.Find(); err != nil {
		warn("%v", err)
		return C.SANE_STATUS_IO_ERROR
	}
	key := C.malloc(1) // never dereferenced; just a unique address to key on
	handles[key] = newHandle()
	*out = C.SANE_Handle(key)
	return C.SANE_STATUS_GOOD
}

//export sane_cx4300_close
func sane_cx4300_close(sh C.SANE_Handle) {
	mu.Lock()
	h := lookup(sh)
	if h != nil {
		delete(handles, unsafe.Pointer(sh))
		C.free(unsafe.Pointer(sh))
	}
	mu.Unlock()
	if h != nil {
		h.cancel()
	}
}

//export sane_cx4300_get_option_descriptor
func sane_cx4300_get_option_descriptor(sh C.SANE_Handle, option C.SANE_Int) *C.SANE_Option_Descriptor {
	mu.Lock()
	defer mu.Unlock()
	if lookup(sh) == nil || option < 0 || int(option) >= len(opts) {
		return nil
	}
	return &opts[option]
}

//export sane_cx4300_control_option
func sane_cx4300_control_option(sh C.SANE_Handle, option C.SANE_Int, action C.SANE_Action,
	value unsafe.Pointer, info *C.SANE_Int) C.SANE_Status {
	mu.Lock()
	defer mu.Unlock()
	h := lookup(sh)
	if h == nil || option < 0 || int(option) >= len(opts) {
		return C.SANE_STATUS_INVAL
	}
	if h.scanning {
		return C.SANE_STATUS_DEVICE_BUSY
	}
	if info != nil {
		*info = 0
	}
	return h.controlOption(int(option), action, value, info)
}

//export sane_cx4300_get_parameters
func sane_cx4300_get_parameters(sh C.SANE_Handle, p *C.SANE_Parameters) C.SANE_Status {
	mu.Lock()
	defer mu.Unlock()
	h := lookup(sh)
	if h == nil || p == nil {
		return C.SANE_STATUS_INVAL
	}
	sp, err := h.params()
	if err != nil {
		warn("%v", err)
		return C.SANE_STATUS_INVAL
	}
	width, height := sp.PixelSize()
	p.format = C.SANE_FRAME_RGB
	if sp.Mode == cx4300.ModeGray {
		p.format = C.SANE_FRAME_GRAY
	}
	p.last_frame = C.SANE_TRUE
	p.depth = 8
	p.pixels_per_line = C.SANE_Int(width)
	p.bytes_per_line = C.SANE_Int(width * sp.Mode.BytesPerPixel())
	p.lines = C.SANE_Int(height)
	return C.SANE_STATUS_GOOD
}

//export sane_cx4300_start
func sane_cx4300_start(sh C.SANE_Handle) C.SANE_Status {
	mu.Lock()
	defer mu.Unlock()
	h := lookup(sh)
	if h == nil {
		return C.SANE_STATUS_INVAL
	}
	if h.scanning {
		return C.SANE_STATUS_DEVICE_BUSY
	}
	p, err := h.params()
	if err != nil {
		warn("%v", err)
		return C.SANE_STATUS_INVAL
	}
	dev, err := cx4300.OpenDevice()
	if err != nil {
		warn("%v", err)
		return statusFor(err)
	}
	width, height := p.PixelSize()
	h.width, h.total, h.sent = width, width*p.Mode.BytesPerPixel()*height, 0
	h.cancelled = false
	h.scanning = true
	h.stream = newStream()

	s := h.stream
	go func() {
		_, _, err := dev.ScanRows(p, func(_ int, row []byte) error {
			_, err := s.Write(row)
			return err
		})
		dev.Close()
		if err != nil {
			warn("%v", err)
		}
		s.finish(err)
	}()
	return C.SANE_STATUS_GOOD
}

//export sane_cx4300_read
func sane_cx4300_read(sh C.SANE_Handle, data *C.SANE_Byte, maxLen C.SANE_Int, length *C.SANE_Int) C.SANE_Status {
	mu.Lock()
	h := lookup(sh)
	if h == nil || data == nil || length == nil || maxLen <= 0 {
		mu.Unlock()
		return C.SANE_STATUS_INVAL
	}
	*length = 0
	if h.cancelled {
		mu.Unlock()
		return C.SANE_STATUS_CANCELLED
	}
	if !h.scanning {
		mu.Unlock()
		return C.SANE_STATUS_EOF
	}
	s, want, left := h.stream, int(maxLen), h.total-h.sent
	if left <= 0 {
		h.finish()
		mu.Unlock()
		return C.SANE_STATUS_EOF
	}
	// Released while waiting for image data, so sane_cancel can still get in.
	mu.Unlock()

	if want > left {
		want = left
	}
	buf := unsafe.Slice((*byte)(unsafe.Pointer(data)), int(maxLen))
	n, err := s.read(buf[:want])

	mu.Lock()
	defer mu.Unlock()
	if h.cancelled {
		return C.SANE_STATUS_CANCELLED
	}
	h.sent += n
	if n > 0 {
		*length = C.SANE_Int(n)
		return C.SANE_STATUS_GOOD
	}
	if err != nil && !errors.Is(err, io.EOF) {
		h.finish()
		return statusFor(err)
	}
	// The device ended the image early - a short read is how it says "that is
	// all", and asking it for more would wedge it. Pad the rest of what the
	// frontend was promised with white, so the file it writes is not truncated
	// half way through a row.
	if h.sent < h.total {
		pad := min(int(maxLen), h.total-h.sent)
		for i := range buf[:pad] {
			buf[i] = 0xff
		}
		h.sent += pad
		*length = C.SANE_Int(pad)
		return C.SANE_STATUS_GOOD
	}
	h.finish()
	return C.SANE_STATUS_EOF
}

//export sane_cx4300_cancel
func sane_cx4300_cancel(sh C.SANE_Handle) {
	mu.Lock()
	h := lookup(sh)
	mu.Unlock()
	if h != nil {
		h.cancel()
	}
}

// cancel stops handing rows to the frontend. The transfer itself is left to run
// to its end in the background: this device locks up until its mains power is
// cut if it is abandoned mid-transfer, so the carriage finishes its sweep and
// the USB claim is released a few seconds later.
func (h *handle) cancel() {
	mu.Lock()
	defer mu.Unlock()
	if !h.scanning {
		return
	}
	h.cancelled = true
	s := h.stream
	s.discard()
	// The handle stays busy until that background sweep ends, which is what
	// stops the next sane_start from talking to a device still mid-transfer.
	go func() {
		s.wait()
		mu.Lock()
		if h.stream == s {
			h.finish()
		}
		mu.Unlock()
	}()
}

// finish marks the end of a scan. The caller holds mu.
func (h *handle) finish() {
	h.scanning = false
	h.stream = nil
}

//export sane_cx4300_set_io_mode
func sane_cx4300_set_io_mode(sh C.SANE_Handle, nonBlocking C.SANE_Bool) C.SANE_Status {
	mu.Lock()
	defer mu.Unlock()
	if lookup(sh) == nil {
		return C.SANE_STATUS_INVAL
	}
	if nonBlocking == C.SANE_TRUE {
		return C.SANE_STATUS_UNSUPPORTED
	}
	return C.SANE_STATUS_GOOD
}

//export sane_cx4300_get_select_fd
func sane_cx4300_get_select_fd(sh C.SANE_Handle, fd *C.SANE_Int) C.SANE_Status {
	return C.SANE_STATUS_UNSUPPORTED
}
