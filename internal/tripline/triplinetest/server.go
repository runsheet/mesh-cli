package triplinetest

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// publicKey is the fake project's DSN key; a request authenticated with
// any other key is refused.
const publicKey = "triplinetestpublickey"

// projectID is the fake project's id, the path segment Tripline's ingest
// routes carry.
const projectID = "1"

// Event is one ingested event, the fields Tripline groups and tags issues
// on.
type Event struct {
	EventID        string
	Message        string
	ExceptionType  string
	ExceptionValue string
	Fingerprint    []string
	Tags           map[string]string
	Level          string
}

// Issue is a group of events sharing a fingerprint, as Tripline groups
// them.
type Issue struct {
	Fingerprint string
	Count       int
	Tags        map[string]string
}

// Server is a fake Tripline ingest: an httptest.Server accepting the
// Sentry envelope protocol at /api/<project>/envelope/ and the legacy
// /api/<project>/store/, recording each event it parses out and grouping
// them into issues by fingerprint.
type Server struct {
	t   testing.TB
	srv *httptest.Server

	mu     sync.Mutex
	events []Event
}

// NewServer starts a fake Tripline ingest, closed when t ends.
func NewServer(t testing.TB) *Server {
	t.Helper()
	s := &Server{t: t}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/"+projectID+"/envelope/", s.handleEnvelope)
	mux.HandleFunc("/api/"+projectID+"/store/", s.handleStore)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)

	return s
}

// DSN is the fake project's DSN, as a client configures itself with.
func (s *Server) DSN() string {
	u, err := url.Parse(s.srv.URL)
	if err != nil {
		s.t.Fatalf("triplinetest: parse server URL: %v", err)
	}
	return fmt.Sprintf("%s://%s@%s/%s", u.Scheme, publicKey, u.Host, projectID)
}

// Events is every event recorded so far, in arrival order.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// Issues groups the recorded events by fingerprint, in the order each
// fingerprint was first seen. Each issue's Tags are its first event's.
func (s *Server) Issues() []Issue {
	s.mu.Lock()
	defer s.mu.Unlock()

	var order []string
	byKey := map[string]*Issue{}
	for _, ev := range s.events {
		key := fingerprintKey(ev)
		issue, ok := byKey[key]
		if !ok {
			issue = &Issue{Fingerprint: key, Tags: ev.Tags}
			byKey[key] = issue
			order = append(order, key)
		}
		issue.Count++
	}

	out := make([]Issue, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out
}

// Wait blocks until at least n events have been recorded, or timeout
// elapses, and reports which. For asserting on a flush that happens off
// the calling goroutine.
func (s *Server) Wait(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		count := len(s.events)
		s.mu.Unlock()
		if count >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *Server) record(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *Server) handleEnvelope(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	body, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, items, err := parseEnvelope(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	for _, item := range items {
		if typ, _ := item.Header["type"].(string); typ != "event" {
			continue
		}
		ev, err := parseEventPayload(item.Payload)
		if err != nil {
			continue
		}
		s.record(ev)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"00000000000000000000000000000000"}`))
}

func (s *Server) handleStore(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	body, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ev, err := parseEventPayload(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.record(ev)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"00000000000000000000000000000000"}`))
}

// authorize checks the request's DSN key, the X-Sentry-Auth header or the
// sentry_key query parameter, against the fake project's. A mismatch
// answers 401 and sends nothing further.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if sentryKey(r) == publicKey {
		return true
	}
	w.WriteHeader(http.StatusUnauthorized)
	return false
}

func sentryKey(r *http.Request) string {
	if auth := r.Header.Get("X-Sentry-Auth"); auth != "" {
		for _, part := range strings.Split(auth, ",") {
			part = strings.TrimSpace(part)
			if key, ok := strings.CutPrefix(part, "sentry_key="); ok {
				return key
			}
		}
	}
	return r.URL.Query().Get("sentry_key")
}

// readBody reads the request body, decompressing it first when
// Content-Encoding says gzip, as Tripline does.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var reader io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}
	return io.ReadAll(reader)
}
