//go:build windows

package cx4300

import (
	"encoding/binary"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WIA drives the scanner through Windows Image Acquisition.
//
// The reverse-engineered SCSI protocol in this package cannot be used on
// Windows: usbscan.sys owns the device, and taking it over would mean replacing
// that driver with WinUSB, which breaks scanning for every other Windows
// application. WIA is the supported route and exposes resolution and scan-area
// properties, so preview and crop work the same way.
type WIA struct {
	progress ProgressFunc
}

// Open returns a Scanner for this platform.
func Open() (Scanner, error) {
	w := &WIA{}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		return nil, fmt.Errorf("cx4300: powershell.exe not found, which the WIA backend needs: %w", err)
	}
	return w, nil
}

// SetProgress implements ProgressReporter. WIA transfers are opaque, so this
// only marks the start and end of a scan.
func (w *WIA) SetProgress(fn ProgressFunc) { w.progress = fn }

func (w *WIA) Close() error { return nil }

// WIA property identifiers.
const (
	wiaXRes    = 6147
	wiaYRes    = 6148
	wiaXPos    = 6149
	wiaYPos    = 6150
	wiaXExtent = 6151
	wiaYExtent = 6152
)

const psHelper = `
param([int]$Dpi = 150, [int]$X = 0, [int]$Y = 0, [int]$W = 0, [int]$H = 0,
      [string]$Out = "", [switch]$IdentifyOnly)
$ErrorActionPreference = 'Stop'
$dm = New-Object -ComObject WIA.DeviceManager
$di = $dm.DeviceInfos | Where-Object { $_.Type -eq 1 } | Select-Object -First 1
if (-not $di) { throw "no WIA scanner is present" }
$name = ""
try { $name = $di.Properties.Item("Name").Value } catch {}
if ($IdentifyOnly) { Write-Output ("NAME " + $name); exit 0 }
$dev = $di.Connect()
$item = $dev.Items.Item(1)
function SetProp($id, $val) {
  foreach ($p in $item.Properties) { if ($p.PropertyID -eq $id) { $p.Value = $val; return } }
}
# Resolution must be set before the extents: extents are in pixels at the
# current resolution.
SetProp 6147 $Dpi
SetProp 6148 $Dpi
SetProp 6149 $X
SetProp 6150 $Y
if ($W -gt 0) { SetProp 6151 $W }
if ($H -gt 0) { SetProp 6152 $H }
$img = $item.Transfer("{B96B3CAE-0728-11D3-9D7B-0000F81EF32E}")
if (Test-Path $Out) { Remove-Item $Out -Force }
$img.SaveFile($Out)
Write-Output ("OK " + $img.Width + " " + $img.Height + " " + $name)
`

const psReset = `
$ErrorActionPreference = 'Stop'
$id = (Get-PnpDevice | Where-Object { $_.InstanceId -like "USB\VID_04B8&PID_083F*MI_00*" } |
       Select-Object -First 1).InstanceId
if (-not $id) { throw "scanner PnP node not found" }
Disable-PnpDevice -InstanceId $id -Confirm:$false
Start-Sleep -Seconds 3
Enable-PnpDevice -InstanceId $id -Confirm:$false
Write-Output "OK"
`

// runPS writes a script to a temp file and runs it, returning its stdout.
func runPS(script string, args ...string) (string, error) {
	f, err := os.CreateTemp("", "cx4300-*.ps1")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	full := append([]string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", f.Name()}, args...)
	out, err := exec.Command("powershell.exe", full...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("%w: %s", err, friendlyWIAError(text))
	}
	return text, nil
}

// friendlyWIAError turns the two WIA failures this scanner actually produces
// into something actionable. The messages are localised by Windows, so match on
// the error codes as well as the English text.
func friendlyWIAError(out string) string {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "0x80210006") || strings.Contains(low, "busy"):
		return out + "\n\nThe scanner is held by another application. Close Epson Scan " +
			"(check for a stuck escndv.exe) or use Reset device, which disables and " +
			"re-enables the scanner's PnP node."
	case strings.Contains(low, "0x80210015") || strings.Contains(low, "not available"),
		strings.Contains(low, "no wia scanner is present"):
		return out + "\n\nWindows cannot see the scanner. If it is attached to WSL via " +
			"usbipd, detach it first (usbipd detach --busid <id>). After a detach the " +
			"device node is often left half-initialised and Windows only picks it up " +
			"again after the USB cable is physically unplugged and replugged."
	case strings.Contains(low, "0x80041001"):
		return out + "\n\nWindows refused to restart the device. This happens when the " +
			"device node was left stale by a usbipd detach; unplug and replug the USB " +
			"cable, which always clears it."
	}
	return out
}

func (w *WIA) Identify() (DeviceInfo, error) {
	out, err := runPS(psHelper, "-IdentifyOnly")
	if err != nil {
		return DeviceInfo{}, fmt.Errorf("cx4300: WIA identify failed: %w", err)
	}
	name := strings.TrimSpace(strings.TrimPrefix(lastLine(out), "NAME"))
	return DeviceInfo{Model: name}, nil
}

func (w *WIA) Scan(p Params) (image.Image, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if w.progress != nil {
		w.progress(0, 1)
	}
	// WIA works in pixels at the selected resolution; our areas are 1/600 inch.
	toPx := func(units int) int { return units * p.DPI / Unit }
	out := filepath.Join(os.TempDir(), "cx4300-scan.bmp")

	if _, err := runPS(psHelper,
		"-Dpi", fmt.Sprint(p.DPI),
		"-X", fmt.Sprint(toPx(p.Area.X)),
		"-Y", fmt.Sprint(toPx(p.Area.Y)),
		"-W", fmt.Sprint(toPx(p.Area.W)),
		"-H", fmt.Sprint(toPx(p.Area.H)),
		"-Out", out); err != nil {
		return nil, fmt.Errorf("cx4300: WIA scan failed: %w", err)
	}
	defer os.Remove(out)

	raw, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("cx4300: reading scanned bitmap: %w", err)
	}
	img, err := decodeBMP(raw)
	if err != nil {
		return nil, err
	}
	if w.progress != nil {
		w.progress(1, 1)
	}
	// Oversampling is not applied here: WIA is driven at the resolution asked
	// for, and the helper gives no access to the planes this would average.
	//
	// WIA is asked for colour whatever the mode: the helper drives the vendor's
	// own dialogue-free path and exposing its intent settings buys nothing when
	// the same luma average is a pixel loop away.
	if p.Mode == ModeGray {
		return ToGray(img), nil
	}
	return img, nil
}

// Reset implements Resetter.
func (w *WIA) Reset() error {
	if _, err := runPS(psReset); err != nil {
		return fmt.Errorf("cx4300: resetting the scanner needs Administrator rights: %w", err)
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// decodeBMP reads the uncompressed 24- or 32-bit BGR bitmaps WIA produces.
// Go's standard library has no BMP decoder and pulling in x/image for one
// format would be the only dependency in this module.
func decodeBMP(b []byte) (image.Image, error) {
	if len(b) < 54 || b[0] != 'B' || b[1] != 'M' {
		return nil, fmt.Errorf("cx4300: not a BMP file")
	}
	dataOff := binary.LittleEndian.Uint32(b[10:])
	width := int(int32(binary.LittleEndian.Uint32(b[18:])))
	height := int(int32(binary.LittleEndian.Uint32(b[22:])))
	bits := int(binary.LittleEndian.Uint16(b[28:]))
	compression := binary.LittleEndian.Uint32(b[30:])
	if compression != 0 {
		return nil, fmt.Errorf("cx4300: compressed BMP (type %d) is not supported", compression)
	}
	if bits != 24 && bits != 32 {
		return nil, fmt.Errorf("cx4300: %d-bit BMP is not supported", bits)
	}
	topDown := height < 0
	if topDown {
		height = -height
	}
	bpp := bits / 8
	stride := (width*bpp + 3) / 4 * 4
	if int(dataOff)+stride*height > len(b) {
		return nil, fmt.Errorf("cx4300: truncated BMP: need %d bytes, have %d",
			int(dataOff)+stride*height, len(b))
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		src := int(dataOff) + y*stride
		dstY := height - 1 - y // BMP rows run bottom-up unless height was negative
		if topDown {
			dstY = y
		}
		row := img.Pix[dstY*img.Stride:]
		for x := 0; x < width; x++ {
			p := src + x*bpp
			row[x*4+0] = b[p+2] // R
			row[x*4+1] = b[p+1] // G
			row[x*4+2] = b[p+0] // B
			row[x*4+3] = 0xff
		}
	}
	return img, nil
}
