// Command escan is a small web UI for scanning on an Epson Stylus CX4300.
//
// It serves a page on localhost with three actions: a preview of the whole
// platen, a crop rectangle dragged over that preview, and a full scan of the
// selection. On Linux it drives the scanner directly over usbfs; on Windows it
// goes through WIA.
package main

import (
	"bytes"
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
	"strings"
	"sync"
	"sync/atomic"
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

	// A short life for the token every request carries, and a long one for the
	// token that silently renews it: a leaked access token is stale within the
	// quarter hour, while a browser at home stays logged in for a year.
	defaultAuthTTL        = 15 * time.Minute
	defaultAuthRefreshTTL = 365 * 24 * time.Hour
)

type server struct {
	out        store
	previewDPI int
	scanDPI    int
	previewMax int
	displayMax int
	authOn     bool

	// hub holds the state every browser renders - progress, preview, crop and
	// last scan - and pushes it out as it changes.
	hub *hub

	// live carries the scan in progress, row by row, to any browser watching.
	live *live

	// scanMu serialises device access. Two concurrent scans wedge this
	// scanner, so every request that touches it holds this lock.
	scanMu sync.Mutex

	// stop is raised by /api/cancel and read by the scan in progress. It only
	// stops the image being kept, never the transfer: see run.
	stop atomic.Bool
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
	authUser := flag.String("auth-user", "",
		"user name for the login page; with auth-password in the settings file, it turns authentication on")
	authTTL := flag.Duration("auth-ttl", defaultAuthTTL,
		"how long a login token is accepted for before it is renewed from the refresh token")
	authRefreshTTL := flag.Duration("auth-refresh-ttl", defaultAuthRefreshTTL,
		"how long a browser stays logged in without typing the password again")
	flag.Parse()

	// The file fills in whatever the command line did not, so a service can be
	// configured entirely from /etc/escan.conf.
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	smbPassword, authPassword, jwtSecret := "", "", ""
	if cfg != nil {
		if err := cfg.apply(flag.CommandLine, explicitFlags(flag.CommandLine)); err != nil {
			log.Fatal(err)
		}
		for _, key := range secretKeys {
			if err := cfg.checkSecret(key); err != nil {
				log.Fatal(err)
			}
		}
		smbPassword = cfg.get("smb-password")
		authPassword = cfg.get("auth-password")
		jwtSecret = cfg.get("jwt-secret")
	}

	guard, err := newAuth(*authUser, authPassword, jwtSecret, *authTTL, *authRefreshTTL)
	if err != nil {
		log.Fatal(err)
	}

	dest, err := openStore(*smbAddress, *smbUser, smbPassword, *smbDomain, *out)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{
		hub:        newHub(),
		live:       newLive(),
		out:        dest,
		previewDPI: *previewDPI,
		scanDPI:    *scanDPI,
		previewMax: *previewMax,
		displayMax: *displayMax,
		authOn:     guard != nil,
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
	// The page and its script are embedded in the binary, which gives them no
	// modification time for a browser to revalidate against - so ask for no
	// caching at all. They are a few kilobytes, and the alternative is a
	// browser quietly running the previous version's script after an upgrade.
	mux.Handle("/", noCache(http.FileServer(http.FS(sub))))
	if guard != nil {
		// /login is the same file the static server would hand out at
		// /login.html; naming it here keeps the address in the redirect, in the
		// page and in openPaths identical.
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, sub, "login.html")
		})
		mux.HandleFunc("/api/login", guard.handleLogin)
		mux.HandleFunc("/api/refresh", guard.handleRefresh)
		mux.HandleFunc("/api/logout", guard.handleLogout)
	}
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/events", s.hub.handleEvents)
	mux.HandleFunc("/api/live", s.handleLive)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/selection", s.handleSelection)
	mux.HandleFunc("/api/image/", s.handleImage)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/file/", s.handleFile)

	var handler http.Handler = mux
	if guard != nil {
		handler = guard.guard(mux)
		log.Printf("authentication is on; log in as %s", *authUser)
	} else {
		log.Printf("WARNING: no auth-user/auth-password configured: anything that can " +
			"reach this address can scan and can download every scan in the output location")
	}

	srv := &http.Server{
		Addr:        *addr,
		Handler:     handler,
		ReadTimeout: 30 * time.Second,
		// No write timeout: a 600 dpi full-bed scan takes minutes over this
		// device's USB 1.1 link.
		WriteTimeout: 0,
	}
	log.Printf("scans will be written to %s", s.out.Describe())
	log.Printf("open http://%s/", *addr)
	log.Fatal(srv.ListenAndServe())
}

// noCache stops a browser holding on to the embedded page and script.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

func (s *server) setProgress(done, total int, stage string) {
	s.hub.change(func() {
		s.hub.snap.Done, s.hub.snap.Total, s.hub.snap.Stage = done, total, stage
	})
}

func (s *server) setBusy(b bool) {
	s.hub.change(func() {
		s.hub.snap.Busy = b
		if !b {
			s.hub.snap.Stage = ""
			s.hub.snap.Done, s.hub.snap.Total = 0, 0
		}
	})
}

// fail records why a scan stopped. It reaches every browser, not just the one
// that started it.
func (s *server) fail(err error) {
	log.Printf("error: %v", err)
	s.hub.change(func() { s.hub.snap.Error = err.Error() })
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
		"auth":        s.authOn,
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
	s.start(w, cx4300.Params{DPI: req.DPI, Area: area}, "preview", s.previewMax)
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
	s.start(w, cx4300.Params{DPI: req.DPI, Area: area}, "scan", s.displayMax)
}

// start accepts a scan and runs it in the background. The browser that asked is
// told only that it was accepted: the image, the progress and any error reach
// every browser the same way, through the shared state, so a scan started on
// one device is fully visible on the others.
func (s *server) start(w http.ResponseWriter, p cx4300.Params, kind string, maxEdge int) {
	if !s.scanMu.TryLock() {
		writeErr(w, http.StatusConflict, fmt.Errorf("a scan is already running"))
		return
	}
	s.stop.Store(false)
	// Announced here rather than when the device answers, so a browser that
	// asks for the stream the moment it sees "busy" finds it waiting.
	fullW, fullH := p.Area.Pixels(p.DPI)
	s.live.start(fullW, fullH, maxEdge)
	s.hub.change(func() {
		s.hub.snap.Busy = true
		s.hub.snap.Error = ""
		s.hub.snap.Stage = "opening scanner"
		s.hub.snap.Done, s.hub.snap.Total = 0, 0
	})
	go func() {
		defer s.scanMu.Unlock()
		defer s.setBusy(false)
		s.run(p, kind, maxEdge)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// run performs one scan. A scan - as opposed to a preview - is saved to disk at
// full resolution; the copy the browsers display is shrunk to maxEdge so a page
// on a phone is not asked to hold a 600 dpi full-bed image.
func (s *server) run(p cx4300.Params, kind string, maxEdge int) {
	defer s.live.finish()

	sc, err := cx4300.Open()
	if err != nil {
		s.fail(err)
		return
	}
	defer sc.Close()

	if pr, ok := sc.(cx4300.ProgressReporter); ok {
		var last time.Time
		pr.SetProgress(func(done, total int) {
			// The device reports every block; browsers get a quarter-second
			// cadence, plus the final block whatever its timing.
			now := time.Now()
			if done < total && now.Sub(last) < progressEvery {
				return
			}
			last = now
			s.setProgress(done, total, s.stage())
		})
	}
	s.setProgress(0, 0, s.stage())

	started := time.Now()
	var img image.Image
	if st, ok := sc.(cx4300.StreamScanner); ok {
		// Linux: the rows reach the browsers as they are scanned.
		img, err = scanStreaming(st, p, s.live, s.stop.Load)
	} else {
		// Windows/WIA hands over a finished image, so there is nothing to
		// watch; the progress readout is all a browser gets.
		s.live.finish()
		img, err = sc.Scan(p)
	}
	if err != nil {
		s.fail(err)
		return
	}
	elapsed := time.Since(started)
	// End the live stream before the finished image is published, so a page
	// puts its canvas back to the preview first and the new image is the last
	// thing drawn on it rather than the first.
	s.live.finish()

	if s.stop.Load() {
		// Nothing is saved or published: what was asked for was not produced.
		// The sweep itself has already run to its end - see handleCancel.
		s.hub.change(func() { s.hub.snap.Error = "scan stopped" })
		return
	}

	s.setProgress(1, 1, "encoding")
	full := img.Bounds()

	var savedName, savedPath string
	if kind == "scan" {
		savedName = fmt.Sprintf("cx4300-%s-%ddpi.png", started.Format("20060102-150405"), p.DPI)
		if err := writePNG(s.out, savedName, img); err != nil {
			log.Printf("could not save %s: %v", savedName, err)
			savedName = ""
		} else {
			savedPath = s.out.Describe() + "/" + savedName
		}
	}

	shown := shrink(img, maxEdge)
	var buf bytes.Buffer
	if err := png.Encode(&buf, shown); err != nil {
		s.fail(fmt.Errorf("encoding the image for the browser: %w", err))
		return
	}
	b := shown.Bounds()
	info := imageInfo{
		W: b.Dx(), H: b.Dy(), FullW: full.Dx(), FullH: full.Dy(),
		DPI: p.DPI, ElapsedMs: elapsed.Milliseconds(), Area: fromArea(p.Area),
		SavedName: savedName, SavedPath: savedPath,
	}
	s.hub.change(func() {
		s.hub.publish(kind, buf.Bytes(), info)
		if kind == "preview" {
			// A new preview replaces the area a crop was drawn on, so the crop
			// goes with it - on every device, not just this one.
			s.hub.snap.Sel = nil
		}
	})
}

// handleCancel stops the scan in progress - as far as this device allows.
//
// The transfer is deliberately left to run to its end: this scanner locks up
// until its mains lead is pulled if it is abandoned part way through one, and a
// lock-up costs the user far more than the half minute of carriage sweep they
// asked to skip. So stopping means the image is dropped and the page is
// released; the scanner finishes quietly, and the stage says so.
func (s *server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if !s.stopScan() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "nothing to stop"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

// stage describes what the device is doing, which is not quite the same as what
// the user asked for: after a stop it keeps scanning, and saying so is the only
// way the wait makes sense to whoever pressed the button.
func (s *server) stage() string {
	if s.stop.Load() {
		return "stopping - the scanner finishes its sweep"
	}
	return "scanning"
}

// stopScan raises the stop flag if a scan is running, and reports whether it
// did. The live view is ended at once, so the page stops drawing rows it is
// not going to keep.
func (s *server) stopScan() bool {
	if !s.hub.state().Busy {
		return false
	}
	if s.stop.Swap(true) {
		return false // already stopping
	}
	s.live.finish()
	s.setProgress(0, 0, s.stage())
	return true
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.state())
}

// handleImage serves the shrunk copy the pages display. An image id is minted
// once and never reused, so the answer can be cached for good.
func (s *server) handleImage(w http.ResponseWriter, r *http.Request) {
	data, ok := s.hub.image(strings.TrimPrefix(r.URL.Path, "/api/image/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	_, _ = w.Write(data)
}

// handleSelection shares the crop. The page sends it in device units, which is
// what makes it meaningful on a screen of a different size.
func (s *server) handleSelection(w http.ResponseWriter, r *http.Request) {
	var sel *area
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&sel); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("malformed selection"))
		return
	}
	if sel != nil && (sel.W <= 0 || sel.H <= 0) {
		sel = nil
	}
	s.hub.change(func() { s.hub.snap.Sel = sel })
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	// Only ever serve a plain file name from the output location. The store
	// enforces this too - it is the layer that builds the path - but refusing
	// here keeps a bad name out of the SMB session entirely.
	if !validName(name) {
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
