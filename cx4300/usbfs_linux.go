//go:build linux

package cx4300

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// usbfs ioctl plumbing. The request numbers encode the size of their argument,
// so they are derived from the Go structs rather than hardcoded, which keeps
// them right on both 32- and 64-bit builds.
const usbfsType = 0x55 // 'U'

func ioR(nr, size uintptr) uintptr  { return 2<<30 | size<<16 | usbfsType<<8 | nr }
func ioWR(nr, size uintptr) uintptr { return 3<<30 | size<<16 | usbfsType<<8 | nr }
func ioNone(nr uintptr) uintptr     { return usbfsType<<8 | nr }

type bulkTransfer struct {
	ep      uint32
	length  uint32
	timeout uint32 // milliseconds
	data    *byte
}

type usbfsIoctl struct {
	ifno int32
	code int32
	data unsafe.Pointer
}

var (
	reqClaimInterface   = ioR(15, 4)
	reqReleaseInterface = ioR(16, 4)
	reqClearHalt        = ioR(21, 4)
	reqBulk             = ioWR(2, unsafe.Sizeof(bulkTransfer{}))
	reqIoctl            = ioWR(18, unsafe.Sizeof(usbfsIoctl{}))
	reqDisconnect       = ioNone(22)
)

// Found describes a discovered scanner.
type Found struct {
	DevNode   string // /dev/bus/usb/001/002
	SysName   string // 1-1.2
	BehindHub bool   // true when the sysfs path shows an intermediate hub
}

// Find locates the scanner on the USB bus. BehindHub is worth surfacing to the
// user: this device's identify step fails when it is not attached directly to a
// root hub.
func Find() (Found, error) {
	matches, _ := filepath.Glob("/sys/bus/usb/devices/*/idVendor")
	for _, vpath := range matches {
		dir := filepath.Dir(vpath)
		if readHexID(vpath) != VendorID {
			continue
		}
		if readHexID(filepath.Join(dir, "idProduct")) != ProductID {
			continue
		}
		bus, err1 := readInt(filepath.Join(dir, "busnum"))
		dev, err2 := readInt(filepath.Join(dir, "devnum"))
		if err1 != nil || err2 != nil {
			continue
		}
		name := filepath.Base(dir)
		return Found{
			DevNode: fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, dev),
			SysName: name,
			// "1-1" hangs off a root hub; "1-1.2" goes through a hub on port 1.
			BehindHub: strings.Contains(name, "."),
		}, nil
	}
	return Found{}, fmt.Errorf("cx4300: no scanner %04x:%04x found on USB. Check it is "+
		"powered on and plugged straight into a root-hub port (not through a hub)",
		VendorID, ProductID)
}

func readHexID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 16, 32)
	if err != nil {
		return -1
	}
	return int(v)
}

func readInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// usbfsTransport speaks bulk USB through /dev/bus/usb.
type usbfsTransport struct {
	f     *os.File
	iface int32
}

// Open returns a ready Scanner for this platform, so callers can be written
// without build tags.
func Open() (Scanner, error) { return OpenDevice() }

// OpenDevice finds the scanner, claims its scanner interface and returns a
// ready Device. It needs write access to the device node: either run as root or
// install the udev rule from install.sh.
func OpenDevice() (*Device, error) {
	found, err := Find()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(found.DevNode, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("cx4300: opening %s: %w (run as root, or install the "+
			"udev rule so the scanner group may access it)", found.DevNode, err)
	}
	t := &usbfsTransport{f: f}
	if err := t.claim(); err != nil {
		f.Close()
		return nil, err
	}
	// A previous run may have left an endpoint halted.
	t.clearHalt(EndpointOut)
	t.clearHalt(EndpointIn)
	return New(t), nil
}

func (t *usbfsTransport) ioctl(req, arg uintptr) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, t.f.Fd(), req, arg)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

// claim takes interface 0, detaching whatever kernel driver holds it if needed.
func (t *usbfsTransport) claim() error {
	iface := uint32(0)
	err := t.ioctl(reqClaimInterface, uintptr(unsafe.Pointer(&iface)))
	if err == nil {
		t.iface = 0
		return nil
	}
	if err != syscall.EBUSY {
		return fmt.Errorf("cx4300: claiming interface 0: %w", err)
	}
	// Something (usblp and friends) holds it; ask the kernel to let go.
	dis := usbfsIoctl{ifno: 0, code: int32(reqDisconnect)}
	if err := t.ioctl(reqIoctl, uintptr(unsafe.Pointer(&dis))); err != nil {
		return fmt.Errorf("cx4300: interface 0 is busy and detaching its driver failed: %w", err)
	}
	if err := t.ioctl(reqClaimInterface, uintptr(unsafe.Pointer(&iface))); err != nil {
		return fmt.Errorf("cx4300: claiming interface 0 after detach: %w", err)
	}
	t.iface = 0
	return nil
}

func (t *usbfsTransport) clearHalt(ep uint8) {
	e := uint32(ep)
	_ = t.ioctl(reqClearHalt, uintptr(unsafe.Pointer(&e)))
}

func (t *usbfsTransport) bulk(ep uint8, p []byte, timeout time.Duration) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	bt := bulkTransfer{
		ep:      uint32(ep),
		length:  uint32(len(p)),
		timeout: uint32(timeout / time.Millisecond),
		data:    &p[0],
	}
	var n uintptr
	var errno syscall.Errno
	for {
		n, _, errno = syscall.Syscall(syscall.SYS_IOCTL, t.f.Fd(), reqBulk, uintptr(unsafe.Pointer(&bt)))
		if errno == syscall.EINTR {
			continue
		}
		break
	}
	runtime.KeepAlive(p)
	if errno != 0 {
		t.clearHalt(ep)
		return 0, errno
	}
	return int(n), nil
}

func (t *usbfsTransport) BulkOut(data []byte, timeout time.Duration) error {
	n, err := t.bulk(EndpointOut, data, timeout)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("short bulk write: %d of %d bytes", n, len(data))
	}
	return nil
}

func (t *usbfsTransport) BulkIn(buf []byte, timeout time.Duration) (int, error) {
	return t.bulk(EndpointIn, buf, timeout)
}

func (t *usbfsTransport) Close() error {
	iface := uint32(t.iface)
	_ = t.ioctl(reqReleaseInterface, uintptr(unsafe.Pointer(&iface)))
	return t.f.Close()
}
