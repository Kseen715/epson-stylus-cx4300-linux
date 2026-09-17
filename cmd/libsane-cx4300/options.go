//go:build linux

package main

/*
#include <stdlib.h>
#include "sane_abi.h"
*/
import "C"

import (
	"bytes"
	"math"
	"strings"
	"unsafe"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

// The option table. SANE requires option 0 to report how many options there
// are; the rest is the conventional layout a frontend expects to find, with
// each group followed by the options in it.
const (
	optCount = iota // option 0, "number of options"
	optModeGroup
	optMode
	optResolution
	optOversample
	optGeometryGroup
	optTLX
	optTLY
	optBRX
	optBRY
	numOptions
)

// The mode option's values, spelled the way SANE frontends expect to find them
// so that a saved XSane or simple-scan setting matches. The device only ever
// scans colour; "Gray" is the luma average of the three planes, computed in
// cx4300 - see cx4300.ModeGray for why that is worth offering.
const (
	modeColorName = "Color"
	modeGrayName  = "Gray"
	// modeValueSize is the buffer SANE passes for the string, long enough for
	// the longest name and its terminator.
	modeValueSize = 8
)

func modeFor(name string) (cx4300.Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "gray", "grey", "grayscale", "greyscale":
		return cx4300.ModeGray, true
	case "color", "colour", "rgb":
		return cx4300.ModeColor, true
	}
	return cx4300.ModeColor, false
}

// modeFromBuffer reads the mode name out of the buffer a frontend passes.
// SANE says it holds a NUL-terminated string of at most the option's size, but
// the read is bounded by that size regardless: this is a C ABI, and a frontend
// that forgets the terminator must not be able to walk us off the end of its
// allocation.
func modeFromBuffer(value unsafe.Pointer) string {
	buf := unsafe.Slice((*byte)(value), modeValueSize)
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		return string(buf[:i])
	}
	return string(buf)
}

func modeName(m cx4300.Mode) string {
	if m == cx4300.ModeGray {
		return modeGrayName
	}
	return modeColorName
}

// buildOptions lays the option table out in C memory, where it stays for the
// life of the process: SANE hands frontends a pointer to each descriptor and
// they may read it at any time, so none of this is ever freed.
func buildOptions() []C.SANE_Option_Descriptor {
	mem := C.calloc(numOptions, C.sizeof_SANE_Option_Descriptor)
	if mem == nil {
		return nil
	}
	opts := unsafe.Slice((*C.SANE_Option_Descriptor)(mem), numOptions)

	o := &opts[optCount]
	o.name = C.CString("")
	o.title = C.CString("Number of options")
	o.desc = C.CString("Read-only option count.")
	o._type = C.SANE_TYPE_INT
	o.size = C.sizeof_SANE_Word
	o.cap = C.SANE_CAP_SOFT_DETECT

	o = &opts[optModeGroup]
	o.title = C.CString("Scan mode")
	o._type = C.SANE_TYPE_GROUP

	o = &opts[optMode]
	o.name = C.CString("mode")
	o.title = C.CString("Mode")
	o.desc = C.CString("Colour, or grey computed from it as the luma average of the " +
		"three colour planes. The device always scans colour; grey is a third of the " +
		"bytes and cancels most of the per-channel noise a grey original picks up.")
	o._type = C.SANE_TYPE_STRING
	o.size = modeValueSize
	o.cap = C.SANE_CAP_SOFT_SELECT | C.SANE_CAP_SOFT_DETECT
	o.constraint_type = C.SANE_CONSTRAINT_STRING_LIST
	setConstraint(o, unsafe.Pointer(modeList()))

	o = &opts[optResolution]
	o.name = C.CString("resolution")
	o.title = C.CString("Resolution")
	o.desc = C.CString("Resolution of the scan, in dots per inch.")
	o._type = C.SANE_TYPE_INT
	o.unit = C.SANE_UNIT_DPI
	o.size = C.sizeof_SANE_Word
	o.cap = C.SANE_CAP_SOFT_SELECT | C.SANE_CAP_SOFT_DETECT
	o.constraint_type = C.SANE_CONSTRAINT_WORD_LIST
	setConstraint(o, unsafe.Pointer(resolutionList()))

	o = &opts[optOversample]
	o.name = C.CString("oversample")
	o.title = C.CString("Oversample below 300 dpi")
	o.desc = C.CString("Below 300 dpi, scan at 300 and average down. The three colour " +
		"planes read different rows, so under 300 dpi they alias fine detail " +
		"differently and thin lines come out fringed with colour; averaging a 300 dpi " +
		"sweep removes it, at the cost of that sweep's time. No effect at 300 dpi or above.")
	o._type = C.SANE_TYPE_BOOL
	o.size = C.sizeof_SANE_Word
	o.cap = C.SANE_CAP_SOFT_SELECT | C.SANE_CAP_SOFT_DETECT

	o = &opts[optGeometryGroup]
	o.title = C.CString("Geometry")
	o._type = C.SANE_TYPE_GROUP

	xRange := newRange(bedWidthMM)
	yRange := newRange(bedHeightMM)
	for _, g := range []struct {
		idx   int
		name  string
		title string
		desc  string
		rng   *C.SANE_Range
	}{
		{optTLX, "tl-x", "Top-left x", "Left edge of the scan area.", xRange},
		{optTLY, "tl-y", "Top-left y", "Top edge of the scan area.", yRange},
		{optBRX, "br-x", "Bottom-right x", "Right edge of the scan area.", xRange},
		{optBRY, "br-y", "Bottom-right y", "Bottom edge of the scan area.", yRange},
	} {
		o = &opts[g.idx]
		o.name = C.CString(g.name)
		o.title = C.CString(g.title)
		o.desc = C.CString(g.desc)
		o._type = C.SANE_TYPE_FIXED
		o.unit = C.SANE_UNIT_MM
		o.size = C.sizeof_SANE_Word
		o.cap = C.SANE_CAP_SOFT_SELECT | C.SANE_CAP_SOFT_DETECT
		o.constraint_type = C.SANE_CONSTRAINT_RANGE
		setConstraint(o, unsafe.Pointer(g.rng))
	}
	return opts
}

func boolToSANE(v bool) C.SANE_Bool {
	if v {
		return C.SANE_TRUE
	}
	return C.SANE_FALSE
}

// setConstraint writes a pointer into the descriptor's constraint union, which
// cgo presents as opaque bytes.
func setConstraint(o *C.SANE_Option_Descriptor, p unsafe.Pointer) {
	*(*unsafe.Pointer)(unsafe.Pointer(&o.constraint[0])) = p
}

// modeList renders the mode names as the NULL-terminated string list a
// SANE_CONSTRAINT_STRING_LIST option points at. Like every other descriptor
// here it is never freed: a frontend may read it for the life of the process.
func modeList() **C.char {
	names := []string{modeColorName, modeGrayName}
	mem := (**C.char)(C.calloc(C.size_t(len(names)+1), C.size_t(unsafe.Sizeof((*C.char)(nil)))))
	list := unsafe.Slice(mem, len(names)+1)
	for i, n := range names {
		list[i] = C.CString(n)
	}
	return mem
}

// resolutionList renders SupportedDPI as a SANE word list: the count, then the
// values.
func resolutionList() *C.SANE_Word {
	n := len(cx4300.SupportedDPI)
	mem := (*C.SANE_Word)(C.calloc(C.size_t(n+1), C.sizeof_SANE_Word))
	list := unsafe.Slice(mem, n+1)
	list[0] = C.SANE_Word(n)
	for i, dpi := range cx4300.SupportedDPI {
		list[i+1] = C.SANE_Word(dpi)
	}
	return mem
}

// newRange is a 0..max millimetre range with no quantisation, so a frontend may
// select any area on the glass.
func newRange(maxMM float64) *C.SANE_Range {
	r := (*C.SANE_Range)(C.calloc(1, C.sizeof_SANE_Range))
	r.min = 0
	r.max = C.SANE_Word(fix(maxMM))
	r.quant = 0
	return r
}

// buildDeviceLists renders the two NULL-terminated lists sane_get_devices
// returns: one holding this scanner, and an empty one for when it is not
// plugged in.
func buildDeviceLists() (found, empty **C.SANE_Device) {
	dev := (*C.SANE_Device)(C.calloc(1, C.sizeof_SANE_Device))
	dev.name = C.CString(deviceName)
	dev.vendor = C.CString("Epson")
	dev.model = C.CString("Stylus CX4300 series")
	dev._type = C.CString("flatbed scanner")

	full := (**C.SANE_Device)(C.calloc(2, C.size_t(unsafe.Sizeof(dev))))
	unsafe.Slice(full, 2)[0] = dev

	none := (**C.SANE_Device)(C.calloc(1, C.size_t(unsafe.Sizeof(dev))))
	return full, none
}

// controlOption implements sane_control_option for one option. The caller holds
// mu and has already range-checked the option number.
func (h *handle) controlOption(option int, action C.SANE_Action, value unsafe.Pointer,
	info *C.SANE_Int) C.SANE_Status {
	switch action {
	case C.SANE_ACTION_GET_VALUE:
		if value == nil {
			return C.SANE_STATUS_INVAL
		}
		switch option {
		case optCount:
			*(*C.SANE_Int)(value) = numOptions
		case optMode:
			name := modeName(h.mode)
			buf := unsafe.Slice((*byte)(value), modeValueSize)
			copy(buf, name)
			buf[len(name)] = 0
		case optResolution:
			*(*C.SANE_Int)(value) = C.SANE_Int(h.dpi)
		case optOversample:
			*(*C.SANE_Bool)(value) = boolToSANE(h.oversample)
		case optTLX, optTLY, optBRX, optBRY:
			*(*C.SANE_Fixed)(value) = *h.geometry(option)
		default:
			return C.SANE_STATUS_INVAL // the groups hold no value
		}
		return C.SANE_STATUS_GOOD

	case C.SANE_ACTION_SET_VALUE:
		if value == nil {
			return C.SANE_STATUS_INVAL
		}
		switch option {
		case optMode:
			got, ok := modeFor(modeFromBuffer(value))
			if !ok {
				return C.SANE_STATUS_INVAL
			}
			h.mode = got
			// The frame format and the row length both change with the mode, so
			// a frontend that caches sane_get_parameters has to ask again.
			setInfo(info, C.SANE_INFO_RELOAD_PARAMS, false)
		case optResolution:
			want := int(*(*C.SANE_Int)(value))
			got := nearestDPI(want)
			h.dpi = got
			*(*C.SANE_Int)(value) = C.SANE_Int(got)
			setInfo(info, C.SANE_INFO_RELOAD_PARAMS, got != want)
		case optOversample:
			h.oversample = *(*C.SANE_Bool)(value) == C.SANE_TRUE
			// The image size does not change, but the time it takes does, and
			// a frontend showing an estimate should ask again.
			setInfo(info, C.SANE_INFO_RELOAD_PARAMS, false)
		case optTLX, optTLY, optBRX, optBRY:
			want := *(*C.SANE_Fixed)(value)
			got := h.setGeometry(option, want)
			*(*C.SANE_Fixed)(value) = got
			setInfo(info, C.SANE_INFO_RELOAD_PARAMS, got != want)
		default:
			return C.SANE_STATUS_INVAL
		}
		return C.SANE_STATUS_GOOD

	case C.SANE_ACTION_SET_AUTO:
		// No option is marked SANE_CAP_AUTOMATIC: there is nothing here the
		// backend could sensibly choose on the frontend's behalf.
		return C.SANE_STATUS_UNSUPPORTED
	}
	return C.SANE_STATUS_INVAL
}

func setInfo(info *C.SANE_Int, flags C.SANE_Int, inexact bool) {
	if info == nil {
		return
	}
	*info |= flags
	if inexact {
		*info |= C.SANE_INFO_INEXACT
	}
}

func (h *handle) geometry(option int) *C.SANE_Fixed {
	switch option {
	case optTLX:
		return &h.tlx
	case optTLY:
		return &h.tly
	case optBRX:
		return &h.brx
	default:
		return &h.bry
	}
}

// setGeometry clamps an edge to the platen and keeps the two edges of each axis
// in order, so that no combination of option writes can produce a negative
// area. It returns the value actually stored.
func (h *handle) setGeometry(option int, want C.SANE_Fixed) C.SANE_Fixed {
	limit := fix(bedWidthMM)
	if option == optTLY || option == optBRY {
		limit = fix(bedHeightMM)
	}
	got := want
	if got < 0 {
		got = 0
	}
	if got > limit {
		got = limit
	}
	*h.geometry(option) = got
	// A frontend usually sets the top-left corner before the bottom-right one,
	// so push the other edge rather than refusing the value.
	switch option {
	case optTLX:
		if h.brx < got {
			h.brx = got
		}
	case optTLY:
		if h.bry < got {
			h.bry = got
		}
	case optBRX:
		if h.tlx > got {
			h.tlx = got
		}
	case optBRY:
		if h.tly > got {
			h.tly = got
		}
	}
	return got
}

// nearestDPI snaps a requested resolution to one the device offers. Frontends
// respect the word list, so this only matters for ones that set a value blind.
func nearestDPI(want int) int {
	best, bestDist := cx4300.SupportedDPI[0], math.MaxInt
	for _, dpi := range cx4300.SupportedDPI {
		if d := abs(dpi - want); d < bestDist {
			best, bestDist = dpi, d
		}
	}
	return best
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// modeNamesFromConstraint reads a string-list constraint back out of a
// descriptor. Only the tests need it: a test file cannot import "C", so the
// pointer walk has to live here.
func modeNamesFromConstraint(o *C.SANE_Option_Descriptor) []string {
	list := *(***C.char)(unsafe.Pointer(&o.constraint[0]))
	var out []string
	for i := 0; ; i++ {
		p := *(**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(list)) +
			uintptr(i)*unsafe.Sizeof((*C.char)(nil))))
		if p == nil {
			return out
		}
		out = append(out, C.GoString(p))
	}
}
