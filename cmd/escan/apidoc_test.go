package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func testServer(t *testing.T) *server {
	t.Helper()
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{hub: newHub(), live: newLive(), web: sub, authOn: true}
	s.api = s.routes(&auth{})
	return s
}

// The table is the only description of this server's HTTP surface, so an entry
// that is missing what the document needs would go out as a silently empty
// section.
func TestRouteTableIsComplete(t *testing.T) {
	s := testServer(t)
	seen := map[string]bool{}
	for _, rt := range s.api {
		if rt.Pattern == "" || rt.Handler == nil {
			t.Fatalf("route %q: pattern and handler are both required", rt.Pattern)
		}
		key := rt.Method + " " + rt.Pattern
		if seen[key] {
			t.Errorf("%s: registered twice; the mux would panic", key)
		}
		seen[key] = true
		if rt.Method != http.MethodGet && rt.Method != http.MethodPost {
			t.Errorf("%s: method must be GET or POST, it is enforced", key)
		}
		if rt.Summary == "" {
			t.Errorf("%s: no summary, so the docs page shows a blank heading", key)
		}
		if strings.HasSuffix(rt.Pattern, "/") && rt.Param == nil {
			t.Errorf("%s: a prefix pattern needs a Param, or the docs hide the path segment", key)
		}
		if rt.Resp == nil && rt.Type == "" {
			t.Errorf("%s: says nothing about what it answers with", key)
		}
	}
	// The endpoints the guard lets through unauthenticated are all declared
	// here except the login page and the icon, which are static files.
	for path := range openPaths {
		if !strings.HasPrefix(path, "/api/") {
			continue
		}
		if !seen[http.MethodPost+" "+path] && !seen[http.MethodGet+" "+path] {
			t.Errorf("%s is open but not in the route table", path)
		}
	}
}

func TestOpenAPIDocument(t *testing.T) {
	s := testServer(t)
	raw, err := json.Marshal(openAPI(s.api, true))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string           `json:"operationId"`
			Security    *[]any           `json:"security"`
			Responses   map[string]any   `json:"responses"`
			Parameters  []map[string]any `json:"parameters"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	for _, rt := range s.api {
		ops, ok := doc.Paths[rt.path()]
		if !ok {
			t.Fatalf("%s missing from the document", rt.path())
		}
		op, ok := ops[strings.ToLower(rt.Method)]
		if !ok {
			t.Fatalf("%s %s missing from the document", rt.Method, rt.path())
		}
		if len(op.Responses) == 0 {
			t.Errorf("%s: no responses", op.OperationID)
		}
		// An open endpoint must say so, and a guarded one must not, or the
		// page tells a reader the wrong thing about logging in.
		open := op.Security != nil && len(*op.Security) == 0
		if open != openPaths[rt.Pattern] {
			t.Errorf("%s: document says open=%v, openPaths says %v",
				op.OperationID, open, openPaths[rt.Pattern])
		}
		if rt.Param != nil && len(op.Parameters) != 1 {
			t.Errorf("%s: path parameter %q is not in the document", op.OperationID, rt.Param.Name)
		}
	}

	// A field renamed in Go must rename itself here; this is the check that
	// the document is generated rather than remembered.
	snap, ok := doc.Components.Schemas["snapshot"]
	if !ok {
		t.Fatal("snapshot schema missing")
	}
	if !strings.Contains(string(snap), `"percent"`) {
		t.Errorf("snapshot schema does not describe percent: %s", snap)
	}
	if strings.Contains(string(raw), `"Percent"`) {
		t.Error("document uses Go field names; the json tags are what the wire carries")
	}
}

func TestSchemaOfReadsTags(t *testing.T) {
	type inner struct {
		N int `json:"n" doc:"a number"`
	}
	type outer struct {
		Name    string  `json:"name" doc:"what it is called"`
		Skipped string  `json:"-"`
		Maybe   *inner  `json:"maybe,omitempty"`
		List    []inner `json:"list"`
		hidden  int
	}
	defs := map[string]any{}
	s := schemaOf(reflect.TypeOf(outer{}), defs)

	// A named struct is registered once and referred to by name.
	ref, _ := s["$ref"].(string)
	if ref != "#/components/schemas/outer" {
		t.Fatalf("want a $ref to outer, got %v", s)
	}
	got, _ := defs["outer"].(map[string]any)
	props, _ := got["properties"].(map[string]any)
	if _, ok := props["Skipped"]; ok {
		t.Error(`a json:"-" field is not on the wire and must not be documented`)
	}
	if _, ok := props["hidden"]; ok {
		t.Error("an unexported field must not be documented")
	}
	name, _ := props["name"].(map[string]any)
	if name["type"] != "string" || name["description"] != "what it is called" {
		t.Errorf("name: %v", name)
	}
	required, _ := got["required"].([]any)
	if len(required) != 2 { // name and list; maybe is omitempty, so it is optional
		t.Errorf("required: %v", required)
	}
	if _, ok := defs["inner"]; !ok {
		t.Error("the nested type was not registered")
	}
	// A pointer to a named struct is that struct or null, and the page renders
	// both; anything else would claim a nil selection cannot happen.
	maybe, _ := props["maybe"].(map[string]any)
	if _, ok := maybe["oneOf"]; !ok {
		t.Errorf("maybe: want oneOf with null, got %v", maybe)
	}
}

func TestMethodIsEnforced(t *testing.T) {
	rt := route{Method: http.MethodPost, Pattern: "/api/scan",
		Handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }}

	w := httptest.NewRecorder()
	rt.serve()(w, httptest.NewRequest(http.MethodGet, "/api/scan", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on a POST endpoint: got %d", w.Code)
	}
	if got := w.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow: %q", got)
	}

	w = httptest.NewRecorder()
	rt.serve()(w, httptest.NewRequest(http.MethodPost, "/api/scan", nil))
	if w.Code != http.StatusTeapot {
		t.Errorf("POST: got %d, want the handler to run", w.Code)
	}

	// HEAD is how a browser and a proxy probe a GET, so it must not be a 405.
	get := route{Method: http.MethodGet, Pattern: "/api/state",
		Handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }}
	w = httptest.NewRecorder()
	get.serve()(w, httptest.NewRequest(http.MethodHead, "/api/state", nil))
	if w.Code != http.StatusTeapot {
		t.Errorf("HEAD on a GET endpoint: got %d", w.Code)
	}
}

func TestServesSpecAndDocs(t *testing.T) {
	s := testServer(t)

	w := httptest.NewRecorder()
	s.handleOpenAPI(w, httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("openapi.json: %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type: %q", got)
	}
	first := w.Body.String()
	if !json.Valid([]byte(first)) {
		t.Fatal("openapi.json is not valid JSON")
	}

	// The document is built once and kept; a second request must be the same
	// bytes rather than a fresh reflection pass.
	w2 := httptest.NewRecorder()
	s.handleOpenAPI(w2, httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil))
	if w2.Body.String() != first {
		t.Error("the cached document changed between requests")
	}

	w3 := httptest.NewRecorder()
	s.handleDocs(w3, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if w3.Code != http.StatusOK {
		t.Fatalf("/docs: %d", w3.Code)
	}
	if !strings.Contains(w3.Body.String(), "/api/openapi.json") {
		t.Error("the docs page does not fetch the document it renders")
	}
}
