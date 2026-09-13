package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func fromArea(a cx4300.Area) area { return area{X: a.X, Y: a.Y, W: a.W, H: a.H} }

// imageInfo describes one image held by the hub. The bytes themselves are
// fetched separately at /api/image/{id}; the id changes with every new image,
// so a browser knows from the snapshot alone whether it is holding the current
// one, and the fetch can be cached forever.
type imageInfo struct {
	ID        string `json:"id"`
	W         int    `json:"w"`
	H         int    `json:"h"`
	FullW     int    `json:"fullW"`
	FullH     int    `json:"fullH"`
	DPI       int    `json:"dpi"`
	ElapsedMs int64  `json:"elapsedMs"`
	// Area is what was scanned, so a selection drawn on a preview maps back to
	// device units without assuming how much the image was shrunk.
	Area      area   `json:"area"`
	SavedName string `json:"savedName,omitempty"`
	SavedPath string `json:"savedPath,omitempty"`
}

type snapshot struct {
	Rev     int     `json:"rev"`
	Busy    bool    `json:"busy"`
	Stage   string  `json:"stage"`
	Done    int     `json:"done"`
	Total   int     `json:"total"`
	Percent float64 `json:"percent"`
	Error   string  `json:"error"`

	Preview *imageInfo `json:"preview"`
	Result  *imageInfo `json:"result"`
	// Last is whichever of the two was produced most recently, for the "last
	// run" readout.
	Last *imageInfo `json:"last"`
	// Sel is the crop, shared so that dragging one out on one device moves it
	// on all of them. Nil means the whole bed.
	Sel *area `json:"sel"`
}

type hub struct {
	mu   sync.Mutex
	snap snapshot
	// png holds the shrunk copies the browsers display, keyed by image id. Only
	// the current preview and the current result are kept.
	png    map[string][]byte
	imgSeq int
	subs   map[chan []byte]struct{}
}

func newHub() *hub {
	return &hub{png: map[string][]byte{}, subs: map[chan []byte]struct{}{}}
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
	info.ID = fmt.Sprintf("%s-%d.png", kind, h.imgSeq)

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
