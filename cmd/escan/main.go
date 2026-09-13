// Command escan is a small web UI for scanning on an Epson Stylus CX4300.
//
// It serves a page on localhost with three actions: a preview of the whole
// platen, a crop rectangle dragged over that preview, and a full scan of the
// selection. On Linux it drives the scanner directly over usbfs; on Windows it
// goes through WIA.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

//go:embed web
var webFS embed.FS

// Defaults, all in one place. Each is also a command-line flag, so these are
// the values you get with no arguments.
const (
	defaultAddr       = "127.0.0.1:8080"
	defaultOutDir     = "."
	defaultPreviewDPI = 75   // lowest the device offers, and the quickest
	defaultScanDPI    = 300  // preselected in the scan resolution menu
	defaultPreviewMax = 900  // longest edge of the preview sent to the browser
	defaultDisplayMax = 1600 // longest edge of a finished scan shown in the browser
)

type server struct {
	out        store
	previewDPI int
	scanDPI    int
	previewMax int
	displayMax int

	// scanMu serialises device access. Two concurrent scans wedge this
	// scanner, so every request that touches it holds this lock.
	scanMu sync.Mutex

	progMu sync.Mutex
	busy   bool
	done   int
	total  int
	stage  string
}

func main() {
	addr := flag.String("addr", defaultAddr, "address to listen on")
	out := flag.String("out", defaultOutDir, "directory to write finished scans into")
	previewDPI := flag.Int("preview-dpi", defaultPreviewDPI,
		"preview resolution; 75 is the lowest the device offers and the quickest")
	scanDPI := flag.Int("scan-dpi", defaultScanDPI,
		"resolution preselected in the scan menu")
	previewMax := flag.Int("preview-max", defaultPreviewMax,
		"longest edge, in pixels, of the preview sent to the browser (0 keeps full size)")
	displayMax := flag.Int("display-max", defaultDisplayMax,
		"longest edge, in pixels, of the finished scan shown in the browser; the file "+
			"saved to disk is always full resolution (0 keeps full size)")
	configPath := flag.String("config", defaultConfigPath,
		"settings file; ignored if it does not exist")
	smbAddress := flag.String("smb-address", "",
		"write scans to an SMB share instead of a local directory, as //host/share[/subdir]")
	smbUser := flag.String("smb-user", "", "user to log in to the SMB share as")
	smbDomain := flag.String("smb-domain", "", "domain or workgroup for the SMB login")
	flag.Parse()

	// The file fills in whatever the command line did not, so a service can be
	// configured entirely from /etc/escan.conf.
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	smbPassword := ""
	if cfg != nil {
		if err := cfg.apply(flag.CommandLine, explicitFlags(flag.CommandLine)); err != nil {
			log.Fatal(err)
		}
		if err := cfg.checkSecret("smb-password"); err != nil {
			log.Fatal(err)
		}
		smbPassword = cfg.get("smb-password")
	}

	dest, err := openStore(*smbAddress, *smbUser, smbPassword, *smbDomain, *out)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{
		out:        dest,
		previewDPI: *previewDPI,
		scanDPI:    *scanDPI,
		previewMax: *previewMax,
		displayMax: *displayMax,
	}
	// Fail at startup rather than on the first scan.
	for _, f := range []struct {
		name string
		dpi  int
	}{{"--preview-dpi", s.previewDPI}, {"--scan-dpi", s.scanDPI}} {
		if err := (cx4300.Params{DPI: f.dpi, Area: cx4300.FullBed()}).Validate(); err != nil {
			log.Fatalf("%s: %v", f.name, err)
		}
	}

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/progress", s.handleProgress)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/file/", s.handleFile)

	srv := &http.Server{
		Addr:        *addr,
		Handler:     mux,
		ReadTimeout: 30 * time.Second,
		// No write timeout: a 600 dpi full-bed scan takes minutes over this
		// device's USB 1.1 link.
		WriteTimeout: 0,
	}
	log.Printf("scans will be written to %s", s.out.Describe())
	log.Printf("open http://%s/", *addr)
	log.Fatal(srv.ListenAndServe())
}

func (s *server) setProgress(done, total int, stage string) {
	s.progMu.Lock()
	s.done, s.total, s.stage = done, total, stage
	s.progMu.Unlock()
}

func (s *server) setBusy(b bool) {
	s.progMu.Lock()
	s.busy = b
	if !b {
		s.stage = ""
	}
	s.progMu.Unlock()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	log.Printf("error: %v", err)
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"backend":     backendName,
		"bedWidthMm":  float64(cx4300.BedWidth) / cx4300.Unit * 25.4,
		"bedHeightMm": float64(cx4300.BedHeight) / cx4300.Unit * 25.4,
		"previewDpi":  s.previewDPI,
		"scanDpi":     s.scanDPI,
		"dpiOptions":  cx4300.SupportedDPI,
		"outDir":      s.out.Describe(),
		"canReset":    false,
	}
	if warn := hubWarning(); warn != "" {
		resp["warning"] = warn
	}

	if !s.scanMu.TryLock() {
		resp["device"] = "busy scanning"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	defer s.scanMu.Unlock()

	sc, err := cx4300.Open()
	if err != nil {
		resp["error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	defer sc.Close()
	if _, ok := sc.(cx4300.Resetter); ok {
		resp["canReset"] = true
	}
	info, err := sc.Identify()
	if err != nil {
		resp["error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["device"] = info.Model
	resp["firmware"] = info.Firmware
	writeJSON(w, http.StatusOK, resp)
}

type scanRequest struct {
	DPI  int  `json:"dpi"`
	X    int  `json:"x"`
	Y    int  `json:"y"`
	W    int  `json:"w"`
	H    int  `json:"h"`
	Full bool `json:"full"`
}

func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	req := scanRequest{DPI: s.previewDPI}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.DPI == 0 {
		req.DPI = s.previewDPI
	}
	// A preview of a chosen region is useful for checking focus before
	// committing to a slow high-resolution pass.
	area := cx4300.FullBed()
	if !req.Full && req.W > 0 && req.H > 0 {
		area = cx4300.Area{X: req.X, Y: req.Y, W: req.W, H: req.H}
	}
	s.run(w, cx4300.Params{DPI: req.DPI, Area: area}, false, s.previewMax)
}

func (s *server) handleScan(w http.ResponseWriter, r *http.Request) {
	var req scanRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.DPI == 0 {
		req.DPI = s.scanDPI
	}
	area := cx4300.FullBed()
	if !req.Full {
		area = cx4300.Area{X: req.X, Y: req.Y, W: req.W, H: req.H}
	}
	s.run(w, cx4300.Params{DPI: req.DPI, Area: area}, true, s.displayMax)
}

// run performs one scan. The full-resolution image is saved to disk when save
// is set; the copy sent to the browser is shrunk to maxEdge so the page stays
// responsive even for a 600 dpi scan.
func (s *server) run(w http.ResponseWriter, p cx4300.Params, save bool, maxEdge int) {
	if !s.scanMu.TryLock() {
		writeErr(w, http.StatusConflict, fmt.Errorf("a scan is already running"))
		return
	}
	defer s.scanMu.Unlock()

	s.setBusy(true)
	defer s.setBusy(false)
	s.setProgress(0, 0, "opening scanner")

	sc, err := cx4300.Open()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	defer sc.Close()

	if pr, ok := sc.(cx4300.ProgressReporter); ok {
		pr.SetProgress(func(done, total int) { s.setProgress(done, total, "scanning") })
	}
	s.setProgress(0, 0, "scanning")

	started := time.Now()
	img, err := sc.Scan(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	elapsed := time.Since(started)

	s.setProgress(1, 1, "encoding")
	full := img.Bounds()

	var savedName string
	if save {
		savedName = fmt.Sprintf("cx4300-%s-%ddpi.png", started.Format("20060102-150405"), p.DPI)
		if err := writePNG(s.out, savedName, img); err != nil {
			log.Printf("could not save %s: %v", savedName, err)
			savedName = ""
		}
	}

	shown := shrink(img, maxEdge)
	b := shown.Bounds()
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("X-Image-Width", fmt.Sprint(b.Dx()))
	h.Set("X-Image-Height", fmt.Sprint(b.Dy()))
	h.Set("X-Full-Width", fmt.Sprint(full.Dx()))
	h.Set("X-Full-Height", fmt.Sprint(full.Dy()))
	h.Set("X-Scan-DPI", fmt.Sprint(p.DPI))
	h.Set("X-Elapsed-Ms", fmt.Sprint(elapsed.Milliseconds()))
	// The area actually scanned, so the page can map a selection back to
	// device units without knowing how much the image was shrunk.
	h.Set("X-Area-X", fmt.Sprint(p.Area.X))
	h.Set("X-Area-Y", fmt.Sprint(p.Area.Y))
	h.Set("X-Area-W", fmt.Sprint(p.Area.W))
	h.Set("X-Area-H", fmt.Sprint(p.Area.H))
	if savedName != "" {
		h.Set("X-Saved-Name", savedName)
		h.Set("X-Saved-Path", s.out.Describe()+"/"+savedName)
	}
	if err := png.Encode(w, shown); err != nil {
		log.Printf("encoding response: %v", err)
	}
}

// shrink scales img down so its longest edge is at most maxEdge, averaging the
// source pixels that fall into each destination pixel. It returns img
// unchanged when no scaling is needed.
func shrink(img image.Image, maxEdge int) image.Image {
	b := img.Bounds()
	longest := b.Dx()
	if b.Dy() > longest {
		longest = b.Dy()
	}
	if maxEdge <= 0 || longest <= maxEdge {
		return img
	}
	// Integer box filter: cheap, dependency-free and good enough for a preview.
	factor := (longest + maxEdge - 1) / maxEdge
	dw, dh := (b.Dx()+factor-1)/factor, (b.Dy()+factor-1)/factor

	src, ok := img.(*image.RGBA)
	if !ok {
		tmp := image.NewRGBA(b)
		draw.Draw(tmp, b, img, b.Min, draw.Src)
		src = tmp
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var rs, gs, bs, n int
			for sy := y * factor; sy < (y+1)*factor && sy < b.Dy(); sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x * factor; sx < (x+1)*factor && sx < b.Dx(); sx++ {
					rs += int(row[sx*4+0])
					gs += int(row[sx*4+1])
					bs += int(row[sx*4+2])
					n++
				}
			}
			if n == 0 {
				continue
			}
			o := y*dst.Stride + x*4
			dst.Pix[o+0] = byte(rs / n)
			dst.Pix[o+1] = byte(gs / n)
			dst.Pix[o+2] = byte(bs / n)
			dst.Pix[o+3] = 0xff
		}
	}
	return dst
}

func writePNG(st store, name string, img image.Image) error {
	f, err := st.Create(name)
	if err != nil {
		return err
	}
	// Closing is what flushes the last of an SMB write, so its error matters
	// as much as the encoder's.
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// handleFile serves a saved scan at full resolution, so the Download link does
// not depend on the shrunk copy the page is displaying.
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/file/")
	// Only ever serve a plain file name from the output directory.
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	f, modTime, err := s.out.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	http.ServeContent(w, r, name, modTime, f)
}

func (s *server) handleProgress(w http.ResponseWriter, r *http.Request) {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	pct := 0.0
	if s.total > 0 {
		pct = float64(s.done) / float64(s.total) * 100
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"busy": s.busy, "done": s.done, "total": s.total,
		"percent": pct, "stage": s.stage,
	})
}

func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	if !s.scanMu.TryLock() {
		writeErr(w, http.StatusConflict, fmt.Errorf("a scan is running"))
		return
	}
	defer s.scanMu.Unlock()

	sc, err := cx4300.Open()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	defer sc.Close()
	rs, ok := sc.(cx4300.Resetter)
	if !ok {
		writeErr(w, http.StatusNotImplemented,
			fmt.Errorf("this backend cannot reset the scanner; cut mains power to it for ~30s instead"))
		return
	}
	if err := rs.Reset(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}
