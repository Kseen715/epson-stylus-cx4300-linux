// Command escan is a small web UI for scanning on an Epson Stylus CX4300.
//
// It serves a page on localhost with three actions: a low-resolution preview of
// the whole platen, a crop rectangle dragged over that preview, and a full
// scan of the selection. On Linux it drives the scanner directly over usbfs; on
// Windows it goes through WIA.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

//go:embed web
var webFS embed.FS

// previewDPI is deliberately the lowest the device supports: a preview only has
// to be good enough to position a crop box, and this keeps it quick.
const previewDPI = 75

type server struct {
	outDir string

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
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	out := flag.String("out", ".", "directory to write finished scans into")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("cannot use output directory %s: %v", *out, err)
	}
	abs, _ := filepath.Abs(*out)
	s := &server{outDir: abs}

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

	srv := &http.Server{
		Addr:        *addr,
		Handler:     mux,
		ReadTimeout: 30 * time.Second,
		// No write timeout: a 600 dpi full-bed scan takes minutes over this
		// device's USB 1.1 link.
		WriteTimeout: 0,
	}
	log.Printf("scans will be written to %s", abs)
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
		"previewDpi":  previewDPI,
		"dpiOptions":  cx4300.SupportedDPI,
		"outDir":      s.outDir,
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
	s.run(w, cx4300.Params{DPI: previewDPI, Area: cx4300.FullBed()}, false)
}

func (s *server) handleScan(w http.ResponseWriter, r *http.Request) {
	var req scanRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.DPI == 0 {
		req.DPI = 150
	}
	area := cx4300.FullBed()
	if !req.Full {
		area = cx4300.Area{X: req.X, Y: req.Y, W: req.W, H: req.H}
	}
	s.run(w, cx4300.Params{DPI: req.DPI, Area: area}, true)
}

// run performs one scan and writes the PNG to the response, saving a copy when
// save is set.
func (s *server) run(w http.ResponseWriter, p cx4300.Params, save bool) {
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

	img, err := sc.Scan(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	s.setProgress(1, 1, "encoding")
	var savedPath string
	if save {
		savedPath = filepath.Join(s.outDir,
			fmt.Sprintf("cx4300-%s-%ddpi.png", time.Now().Format("20060102-150405"), p.DPI))
		if err := writePNG(savedPath, img); err != nil {
			log.Printf("could not save %s: %v", savedPath, err)
			savedPath = ""
		}
	}

	b := img.Bounds()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Image-Width", fmt.Sprint(b.Dx()))
	w.Header().Set("X-Image-Height", fmt.Sprint(b.Dy()))
	w.Header().Set("X-Scan-DPI", fmt.Sprint(p.DPI))
	if savedPath != "" {
		w.Header().Set("X-Saved-Path", savedPath)
	}
	if err := png.Encode(w, img); err != nil {
		log.Printf("encoding response: %v", err)
	}
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
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
