// Package cx4300 talks to the scanner half of Epson Stylus CX4300-family
// all-in-ones over USB (vendor 0x04b8, product 0x083f, which also covers the
// CX4400, CX5500, CX5600, DX4400 and DX4450).
//
// # Why this package exists
//
// These devices do not speak ESC/I, despite being handled by Epson's ESC/I
// based "Image Scan! for Linux" driver. Interface 0 speaks SCSI scanner CDBs
// wrapped in a one-byte status protocol. Every command is a CDB written to the
// bulk OUT endpoint followed by an 8-byte status frame on bulk IN whose first
// byte selects the phase:
//
//	StatusSend (0xf8)  the device wants the data-out phase; write it
//	StatusData (0xf9)  data is available; read it, then read a trailing status
//	StatusDone (0xfb)  the command is complete
//
// # Two hardware traps
//
// Both cost a lot of debugging time and neither is a software bug.
//
// First, SANE's epkowa backend opens by probing with ESC/I ("1b 66"). That is
// an invalid CDB here, and receiving it latches the device into answering
// StatusDone to every subsequent command until mains power is removed.
// Re-plugging USB and re-enumerating do not clear it. So a single "scanimage
// -L" poisons the scanner for the rest of its power session, and Scan reports
// this as ErrLatched.
//
// Second, the device must be attached directly to a USB root hub. Behind any
// hub - including the internal hubs on many USB 3 add-in cards and Intel
// rate-matching hubs - the identify step fails and the scan area reads back as
// zero.
//
// # Layering
//
// Device implements the protocol over any Transport, so the protocol can be
// driven through usbfs (the Open constructor on Linux), libusb, gousb, usbip,
// or a fake for testing. See PROTOCOL.md in the repository root for the wire
// format this package implements.
package cx4300
