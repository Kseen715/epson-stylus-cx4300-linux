package main

import (
	"encoding/json"
	"image"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func recv(t *testing.T, ch <-chan []byte) snapshot {
	t.Helper()
	select {
	case msg := <-ch:
		var s snapshot
		if err := json.Unmarshal(msg, &s); err != nil {
			t.Fatalf("decoding a snapshot: %v", err)
		}
		return s
	default:
		t.Fatal("no snapshot was sent")
		return snapshot{}
	}
}

// Every browser gets every change: that is the whole point of the hub.
func TestChangeReachesEverySubscriber(t *testing.T) {
	h := newHub()
	a, _, closeA := h.subscribe()
	b, _, closeB := h.subscribe()
	defer closeA()
	defer closeB()

	h.change(func() { h.snap.Busy = true; h.snap.Done, h.snap.Total = 1, 4 })

	for name, ch := range map[string]<-chan []byte{"a": a, "b": b} {
		got := recv(t, ch)
		if !got.Busy || got.Percent != 25 {
			t.Errorf("subscriber %s saw busy=%v percent=%v, want true and 25", name, got.Busy, got.Percent)
		}
	}
}

// A browser that has not read the frame it was sent still ends up with the
// current state rather than a stale one, because every frame is the whole
// state and the newest simply replaces the oldest.
func TestSlowSubscriberGetsTheNewestState(t *testing.T) {
	h := newHub()
	ch, _, stop := h.subscribe()
	defer stop()

	h.change(func() { h.snap.Stage = "first" })
	h.change(func() { h.snap.Stage = "second" })
	h.change(func() { h.snap.Stage = "third" })

	if got := recv(t, ch).Stage; got != "third" {
		t.Errorf("stage %q, want %q", got, "third")
	}
}

// A stream opens with the state as it stands, so a page that loads mid-scan is
// not blank until the next change.
func TestSubscribeStartsWithTheCurrentState(t *testing.T) {
	h := newHub()
	h.change(func() { h.snap.Stage = "scanning" })

	_, first, stop := h.subscribe()
	defer stop()
	var s snapshot
	if err := json.Unmarshal(first, &s); err != nil {
		t.Fatalf("decoding the opening snapshot: %v", err)
	}
	if s.Stage != "scanning" {
		t.Errorf("stage %q, want %q", s.Stage, "scanning")
	}
}

// Publishing keeps one image per kind: the old bytes go, so a long session does
// not accumulate every preview ever taken.
func TestPublishReplacesTheImageOfThatKind(t *testing.T) {
	h := newHub()
	h.change(func() { h.publish("preview", []byte("one"), imageInfo{W: 10, H: 10}) })
	old := h.state().Preview.ID
	h.change(func() { h.publish("preview", []byte("two"), imageInfo{W: 20, H: 20}) })

	if _, ok := h.image(old); ok {
		t.Error("the replaced preview is still held")
	}
	cur := h.state().Preview
	data, ok := h.image(cur.ID)
	if !ok || string(data) != "two" {
		t.Errorf("current preview is %q (found=%v), want %q", data, ok, "two")
	}
	if cur.ID == old {
		t.Error("the new preview reused the old id; browsers would not refetch it")
	}
}

// The crop is shared state too, in device units.
func TestSelectionIsShared(t *testing.T) {
	s := &server{hub: newHub()}
	ch, _, stop := s.hub.subscribe()
	defer stop()

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleSelection(w, httptest.NewRequest(http.MethodPost, "/api/selection", strings.NewReader(body)))
		return w
	}
	if w := post(`{"x":10,"y":20,"w":30,"h":40}`); w.Code != http.StatusOK {
		t.Fatalf("setting a selection: HTTP %d", w.Code)
	}
	if got := recv(t, ch).Sel; got == nil || *got != (area{X: 10, Y: 20, W: 30, H: 40}) {
		t.Fatalf("shared selection %+v, want {10 20 30 40}", got)
	}

	// An empty rectangle is how the page clears the crop.
	if w := post(`{"x":10,"y":20,"w":0,"h":0}`); w.Code != http.StatusOK {
		t.Fatalf("clearing the selection: HTTP %d", w.Code)
	}
	if got := recv(t, ch).Sel; got != nil {
		t.Errorf("selection %+v after clearing, want none", got)
	}
}

// A restarted server must never mint an image id an earlier run already used:
// those ids are served to the browser as immutable and cached for a year, so a
// repeated id shows the previous run's picture in place of the new scan.
func TestImageIDsDifferBetweenRuns(t *testing.T) {
	first, second := newHub(), newHub()
	ids := map[string]bool{}
	for _, h := range []*hub{first, second} {
		for i := 0; i < 3; i++ {
			h.change(func() { h.publish("preview", []byte{1}, imageInfo{}) })
			id := h.state().Preview.ID
			if ids[id] {
				t.Fatalf("image id %q was minted twice", id)
			}
			ids[id] = true
		}
	}
}

// A grey scan is kept as image.Gray, and shrink is the one place that asserts a
// concrete image type - so it has to handle both.
func TestShrinkAcceptsGray(t *testing.T) {
	src := image.NewGray(image.Rect(0, 0, 8, 4))
	for i := range src.Pix {
		src.Pix[i] = 120
	}
	got := shrink(src, 4)
	b := got.Bounds()
	if b.Dx() != 4 || b.Dy() != 2 {
		t.Fatalf("shrunk to %dx%d, want 4x2", b.Dx(), b.Dy())
	}
	r, g, bl, _ := got.At(0, 0).RGBA()
	if r>>8 != 120 || g>>8 != 120 || bl>>8 != 120 {
		t.Errorf("flat grey 120 became %d,%d,%d", r>>8, g>>8, bl>>8)
	}
}
