// Package meshapitest holds the Mesh v1 contract fixtures' reader, a
// server that replays them, and a stateful fake of the node API built on
// them, for meshapi's and nodev1's tests. The fixtures are
// internal/meshapi/testdata: mesh-cli's copy of projectmesh-io/api's
// contracts/v1, one request and answer per file.
package meshapitest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Fixture is one exchange of the contract.
type Fixture struct {
	Name   string `json:"name"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Caller is who asks: product, node, session, latchkey or none.
	Caller   string          `json:"caller,omitempty"`
	Request  json.RawMessage `json:"request"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
	// ResponseHeaders are the Mesh-* headers answered (a take's
	// Mesh-Waiting), under the key contracts/v1 writes them: headers.
	ResponseHeaders map[string]string `json:"headers,omitempty"`
}

// HasRequest says whether the fixture names a request body.
func (f Fixture) HasRequest() bool { return len(f.Request) > 0 && string(f.Request) != "null" }

// HasResponse says whether the fixture names a response body.
func (f Fixture) HasResponse() bool { return len(f.Response) > 0 && string(f.Response) != "null" }

// Dir is the fixtures' directory, internal/meshapi/testdata.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata")
}

// Load reads every fixture, by name.
func Load(t testing.TB) map[string]Fixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(Dir(), "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("meshapitest: no fixtures in %s: %v", Dir(), err)
	}
	out := map[string]Fixture{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var f Fixture
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("meshapitest: %s: %v", p, err)
		}
		if f.Name != strings.TrimSuffix(filepath.Base(p), ".json") {
			t.Fatalf("meshapitest: %s names itself %q", p, f.Name)
		}
		out[f.Name] = f
	}
	return out
}

// SameJSON says whether a and b are the same JSON value, key order and
// spacing aside, and answers a's and b's canonical text when not.
func SameJSON(a, b []byte) (bool, string, string) {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false, "invalid: " + err.Error(), string(b)
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false, string(a), "invalid: " + err.Error()
	}
	if reflect.DeepEqual(va, vb) {
		return true, "", ""
	}
	ca, _ := json.Marshal(va)
	cb, _ := json.Marshal(vb)
	return false, string(ca), string(cb)
}

// Replay is a server that answers each fixture's request with its
// response and refuses (400) a request whose body is not the fixture's.
// Only fixtures named by Use are served, so one path may have several.
type Replay struct {
	mu      sync.Mutex
	current map[string]Fixture
	// Mismatches are the requests that did not match their fixture.
	Mismatches []string
	// Auth is every request's Authorization header.
	Auth []string
}

// Use serves f for its method and path, replacing any fixture there.
func (r *Replay) Use(f Fixture) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		r.current = map[string]Fixture{}
	}
	r.current[f.Method+" "+f.Path] = f
}

func (r *Replay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	f, ok := r.current[req.Method+" "+req.URL.Path]
	r.Auth = append(r.Auth, req.Header.Get("Authorization"))
	r.mu.Unlock()
	if !ok {
		http.Error(w, "meshapitest: no fixture for "+req.Method+" "+req.URL.Path, http.StatusNotFound)
		return
	}
	if f.HasRequest() {
		var body []byte
		if req.Body != nil {
			body, _ = readAll(req)
		}
		if same, got, want := SameJSON(body, f.Request); !same {
			r.mu.Lock()
			r.Mismatches = append(r.Mismatches, fmt.Sprintf("%s: sent %s, the contract has %s", f.Name, got, want))
			r.mu.Unlock()
			http.Error(w, "meshapitest: the request is not the fixture's", http.StatusBadRequest)
			return
		}
	}
	WriteFixture(w, f)
}

// WriteFixture answers f's status, headers and response. A node's
// event stream (GET …/events) is server-sent events: its fixture's
// response is {"events":[{event, data}]}, written as SSE frames.
func WriteFixture(w http.ResponseWriter, f Fixture) {
	if f.Method == http.MethodGet && strings.HasSuffix(f.Path, "/events") && f.Status == http.StatusOK {
		writeStream(w, f)
		return
	}
	keys := make([]string, 0, len(f.ResponseHeaders))
	for k := range f.ResponseHeaders {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w.Header().Set(k, f.ResponseHeaders[k])
	}
	if f.HasResponse() {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(f.Status)
	if f.HasResponse() && f.Status != http.StatusNoContent {
		w.Write(f.Response)
	}
}

func readAll(req *http.Request) ([]byte, error) {
	defer req.Body.Close()
	return io.ReadAll(req.Body)
}

// writeStream answers f's events as server-sent events.
func writeStream(w http.ResponseWriter, f Fixture) {
	var stream struct {
		Events []struct {
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
		} `json:"events"`
	}
	json.Unmarshal(f.Response, &stream)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": open\n\n")
	for _, e := range stream.Events {
		var compact bytes.Buffer
		json.Compact(&compact, e.Data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Event, compact.Bytes())
	}
}
