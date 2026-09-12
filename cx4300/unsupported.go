//go:build !linux && !windows

package cx4300

import "runtime"

// Open reports that this platform has no backend. The protocol itself is
// portable: construct a Device with New and your own Transport to use it
// anywhere libusb (or equivalent) is available.
func Open() (Scanner, error) {
	return nil, &unsupportedError{}
}

type unsupportedError struct{}

func (*unsupportedError) Error() string {
	return "cx4300: no built-in backend for " + runtime.GOOS +
		" (Linux uses usbfs, Windows uses WIA). The protocol is portable: " +
		"pass your own Transport to cx4300.New"
}
