//go:build linux

package main

import "github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"

const backendName = "usbfs (direct SCSI)"

// hubWarning reports the one topology mistake that makes this scanner fail in a
// way that looks like a driver problem.
func hubWarning() string {
	f, err := cx4300.Find()
	if err != nil || !f.BehindHub {
		return ""
	}
	return "The scanner is at " + f.SysName + ", which is behind a USB hub. " +
		"This device's identify step fails behind a hub - plug it straight into a " +
		"root-hub port."
}
