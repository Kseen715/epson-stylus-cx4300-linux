package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

// There is one scanner, so there is one shared view of it: what it is doing,
// how far along it is, the preview it produced, the crop drawn on that preview
// and the last finished scan. Every browser renders that state and nothing
// else, so a scan started on a phone shows its progress and its result on the
// laptop next to it.
//
// The state lives here in memory, is versioned, and is pushed to every
// connected browser over server-sent events as a whole snapshot. Whole
// snapshots rather than deltas is what keeps a browser that reconnects, or one
// that missed a frame while backgrounded, correct without any catch-up
// protocol.

// How often progress from the device is pushed out. The backend reports every
// block read, which is far more often than a browser can use.
const progressEvery = 250 * time.Millisecond

// heartbeatEvery keeps idle event streams alive through proxies that close a
// connection that has been quiet.
const heartbeatEvery = 25 * time.Second

// area is the platen rectangle as the page sees it, in the device's 1/600 inch
// units. cx4300.Area is the same rectangle without the JSON names.
type area struct {
	X int `json:"x" doc:"left edge, in units of 1/600 inch from the top left of the platen"`
	Y int `json:"y" doc:"top edge, in units of 1/600 inch"`
	W int `json:"w" doc:"width, in units of 1/600 inch"`
	H int `json:"h" doc:"height, in units of 1/600 inch"`
}

func fromArea(a cx4300.Area) area { return area{X: a.X, Y: a.Y, W: a.W, H: a.H} }

// imageInfo describes one image held by the hub. The bytes themselves are
// fetched separately at /api/image/{id}; the id changes with every new image,
// so a browser knows from the snapshot alone whether it is holding the current
// one, and the fetch can be cached forever.
type imageInfo struct {
	ID        string `json:"id" doc:"fetch the displayed copy at /api/image/{id}; never reused, so cacheable for good"`
	W         int    `json:"w" doc:"width of the displayed copy, in pixels"`
	H         int    `json:"h" doc:"height of the displayed copy, in pixels"`
	FullW     int    `json:"fullW" doc:"width of the scan as the device produced it, in pixels"`
	FullH     int    `json:"fullH" doc:"height of the scan as the device produced it, in pixels"`
	DPI       int    `json:"dpi" doc:"resolution this image was scanned at"`
	ElapsedMs int64  `json:"elapsedMs" doc:"how long the scan took, in milliseconds"`
	// Area is what was scanned, so a selection drawn on a preview maps back to
	// device units without assuming how much the image was shrunk.
	Area      area   `json:"area" doc:"the part of the platen this image covers"`
	SavedName string `json:"savedName,omitempty" doc:"file name in the output location; fetch it at /api/file/{name}"`
	SavedPath string `json:"savedPath,omitempty" doc:"where that file was written, as the server sees it"`
}

type snapshot struct {
	Rev     int     `json:"rev" doc:"bumped on every change; a snapshot with a lower rev is stale"`
	Busy    bool    `json:"busy" doc:"whether the scanner is working"`
	Stage   string  `json:"stage" doc:"what it is doing, in words meant for a person"`
	Done    int     `json:"done" doc:"rows transferred so far"`
	Total   int     `json:"total" doc:"rows expected in total"`
	Percent float64 `json:"percent" doc:"progress, 0 to 100"`
	Error   string  `json:"error" doc:"why the last scan stopped; empty when nothing went wrong"`

	Preview *imageInfo `json:"preview" doc:"the current preview, or null"`
	Result  *imageInfo `json:"result" doc:"the last finished scan, or null"`
	// Last is whichever of the two was produced most recently, for the "last
	// run" readout.
	Last *imageInfo `json:"last" doc:"whichever of the two was produced most recently, or null"`
	// Sel is the crop, shared so that dragging one out on one device moves it
	// on all of them. Nil means the whole bed.
	Sel *area `json:"sel" doc:"the shared crop, or null for the whole bed"`
}

type hub struct {
	mu   sync.Mutex
	snap snapshot
	// png holds the shrunk copies the browsers display, keyed by image id. Only
	// the current preview and the current result are kept.
	png    map[string][]byte
	imgSeq int
	subs   map[chan []byte]struct{}
	// run distinguishes this process's image ids from those of any earlier
	// one. See newRunID.
	run string
}

func newHub() *hub {
	return &hub{png: map[string][]byte{}, subs: map[chan []byte]struct{}{}, run: newRunID()}
}

// newRunID returns a token unique to this process, which goes into every image
// id. Without it a restarted server starts counting from one again and mints
// "preview-1.png" a second time - and since those ids are served as immutable
// and cached by the browser for a year, the page would answer a fresh preview
// with the picture from the previous run.
func newRunID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// change runs fn with the hub locked, then bumps the revision and sends the new
// snapshot to every connected browser. It is the only way the state is
// modified, so a browser can never see half an update. fn must not call back
// into the hub.
func (h *hub) change(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn()
	h.snap.Rev++
	h.snap.Percent = 0
	if h.snap.Total > 0 {
		h.snap.Percent = float64(h.snap.Done) / float64(h.snap.Total) * 100
	}
	msg, err := json.Marshal(h.snap)
	if err != nil {
		log.Printf("encoding state: %v", err)
		return
	}
	for c := range h.subs {
		send(c, msg)
	}
}

// send never blocks on a slow browser. Every message is the whole state, so
// dropping the frame it has not read yet in favour of this one loses nothing
// but an intermediate step.
func send(c chan []byte, msg []byte) {
	select {
	case c <- msg:
		return
	default:
	}
	select {
	case <-c:
	default:
	}
	select {
	case c <- msg:
	default:
	}
}

// publish stores an image and points the state at it, replacing whichever image
// of that kind was there before. Call it from inside change.
func (h *hub) publish(kind string, data []byte, info imageInfo) {
	h.imgSeq++
	info.ID = fmt.Sprintf("%s-%s-%d.png", kind, h.run, h.imgSeq)

	slot := &h.snap.Result
	if kind == "preview" {
		slot = &h.snap.Preview
	}
	if *slot != nil {
		delete(h.png, (*slot).ID)
	}
	h.png[info.ID] = data
	*slot = &info
	h.snap.Last = &info
}

func (h *hub) image(id string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.png[id]
	return b, ok
}

func (h *hub) state() snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap
}

// subscribe registers a browser and hands back the current state, so a stream
// starts with a full picture rather than waiting for the next change.
func (h *hub) subscribe() (<-chan []byte, []byte, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := make(chan []byte, 1)
	h.subs[c] = struct{}{}
	first, err := json.Marshal(h.snap)
	if err != nil {
		first = []byte("{}")
	}
	return c, first, func() {
		h.mu.Lock()
		delete(h.subs, c)
		h.mu.Unlock()
	}
}

func (h *hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, first, cancel := h.subscribe()
	defer cancel()

	head := w.Header()
	head.Set("Content-Type", "text/event-stream")
	head.Set("Cache-Control", "no-cache")
	head.Set("Connection", "keep-alive")
	// Nothing here is useful once it is stale, and a buffering reverse proxy
	// would hold a whole scan's progress back until the scan finished.
	head.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	write := func(msg []byte) bool {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !write(first) {
		return
	}

	beat := time.NewTicker(heartbeatEvery)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if !write(msg) {
				return
			}
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
