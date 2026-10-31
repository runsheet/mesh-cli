package egress

import (
	"bytes"
	"io"
	"net/http"
	"sync/atomic"
)

// Canaries (mesh-specs gap 5 item 4, gap 28 item 5): the values a
// lease's relay injects never reach its sandbox, so one seen in a request
// the sandbox sends is a leak. A Tunnel with an Inspector shows it every
// terminated request before any rule writes to it: the URL and headers
// as the session sent them, and its body as it streams upstream.

// CodeExposed is the relay's refusal of a request that carried an
// injected value where it must not go.
const CodeExposed = "credential_exposed"

// Inspector sees every request of a terminated tunnel, as the session
// sent it.
type Inspector interface {
	// Inspect sees req's URL and headers before any rule; an error (a
	// *Refusal says how) refuses it.
	Inspect(req *http.Request) error
	// Body wraps req's body as it streams upstream: a ScanBody.
	Body(req *http.Request, body io.ReadCloser) io.ReadCloser
}

// exposer is a body that found what it scans for.
type exposer interface{ Exposed() bool }

// Find says which watched value b holds: its label, or false.
type Find func(b []byte) (string, bool)

// ScanBody is body scanned for find's values as it streams: a rolling
// window holds back the last window()-1 bytes until the next read rules
// out a value split across reads, so a value never leaves whole, and
// nothing more is held. On a find, found hears the label once and the
// read fails, cutting the request short.
func ScanBody(body io.ReadCloser, find Find, window func() int, found func(label string)) io.ReadCloser {
	return &scanBody{rc: body, find: find, window: window, found: found}
}

type scanBody struct {
	rc      io.ReadCloser
	find    Find
	window  func() int
	found   func(string)
	pending []byte // read, scanned, not yet released: at most window-1 bytes past a read
	out     []byte // released, not yet handed on
	buf     []byte
	err     error
	exposed atomic.Bool
}

// errExposed cuts a request short.
type errExposed struct{ label string }

func (e *errExposed) Error() string {
	return "relay: an injected value was seen in the request body (" + e.label + ")"
}

func (s *scanBody) Exposed() bool { return s.exposed.Load() }

func (s *scanBody) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.err != nil {
			if len(s.pending) > 0 && !s.exposed.Load() {
				s.out, s.pending = s.pending, nil
				break
			}
			return 0, s.err
		}
		if s.buf == nil {
			s.buf = make([]byte, 32<<10)
		}
		n, err := s.rc.Read(s.buf)
		s.pending = append(s.pending, s.buf[:n]...)
		if label, ok := s.find(s.pending); ok {
			s.exposed.Store(true)
			s.pending = nil
			s.err = &errExposed{label: label}
			if s.found != nil {
				s.found(label)
			}
			return 0, s.err
		}
		if err != nil {
			s.err = err
			continue
		}
		keep := max(s.window()-1, 0)
		if len(s.pending) > keep {
			cut := len(s.pending) - keep
			s.out = append([]byte(nil), s.pending[:cut]...)
			s.pending = append([]byte(nil), s.pending[cut:]...)
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

func (s *scanBody) Close() error { return s.rc.Close() }

// FindIn is a Find over values: the label of the first value b holds.
func FindIn(values func() map[string]string) Find {
	return func(b []byte) (string, bool) {
		for v, label := range values() {
			if v != "" && bytes.Contains(b, []byte(v)) {
				return label, true
			}
		}
		return "", false
	}
}
