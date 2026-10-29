package gitrelay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server is the node's one loopback listener for its git relays: each
// registration is a job's Relay behind its own unguessable path,
// http://127.0.0.1:<port>/j/<capability>/git/…, so only the git commands
// the node runs for that job — told the URL by their environment — reach
// it, and each reaches only its own policy.
type Server struct {
	// Upstream and HTTP are every Relay's (GitHub, a plain client).
	Upstream string
	HTTP     *http.Client
	// Audit takes every relay's events.
	Audit func(Event)
	// Counts, when set, counts every relay's events by repository,
	// service and outcome.
	Counts *Counts
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu      sync.Mutex
	ln      net.Listener
	srv     *http.Server
	entries map[string]*entry
}

// entry is one capability: a token-keyed registration (Register, the
// legacy node's) with its relay and expiry, or a lease's grant (Open),
// which lives until it is closed.
type entry struct {
	relay   *Relay
	expires time.Time
	key     string
	grant   *Grant
}

// live says whether the entry still serves at now.
func (e *entry) live(now time.Time) bool {
	return e.grant != nil || e.expires.After(now)
}

// Start listens on a loopback port until ctx is done.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.entries = map[string]*entry{}
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second}
	s.mu.Unlock()
	go s.srv.Serve(ln)
	go func() {
		<-ctx.Done()
		s.srv.Close()
	}()
	return nil
}

// Register serves policy on token until it expires — the legacy node's
// token-keyed registration; a Mesh v1 lease opens a Grant instead and answers the base
// URL a git command's insteadOf points GitHub at:
// http://127.0.0.1:<port>/j/<capability>/git/. The same token and
// policy again answer the same base.
func (s *Server) Register(policy Policy, token Token) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return "", errors.New("gitrelay: the server is not started")
	}
	now := s.now()
	key := token.Value + "\x00" + policy.Repo + "\x00" + string(policy.Access)
	for _, r := range policy.Push {
		key += fmt.Sprintf("\x00%s %t", r.Ref, r.CreateOnly)
	}
	for c, e := range s.entries {
		if e.grant != nil {
			continue
		}
		if !e.expires.After(now) {
			delete(s.entries, c)
			continue
		}
		if e.key == key {
			return s.base(c), nil
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	c := hex.EncodeToString(b[:])
	tok := token
	s.entries[c] = &entry{key: key, expires: token.ExpiresAt, relay: &Relay{
		Policy: policy, Upstream: s.Upstream, HTTP: s.HTTP, Audit: s.audit, Now: s.Now,
		Mint: func(context.Context) (Token, error) { return tok, nil },
		// the node renews the job's token and registers the new one; a
		// relay serves its own until it expires
		RefreshBefore: time.Nanosecond,
	}}
	return s.base(c), nil
}

func (s *Server) base(capability string) string {
	return "http://" + s.ln.Addr().String() + "/j/" + capability + Prefix
}

// ServeHTTP hands /j/<capability>/git/… to that registration's relay.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/j/")
	c, path, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	e := s.entries[c]
	s.mu.Unlock()
	if e == nil || !e.live(s.now()) {
		http.Error(w, "gitrelay: no such relay, or its token expired", http.StatusNotFound)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + path
	if e.grant != nil {
		e.grant.ServeHTTP(w, r2)
		return
	}
	e.relay.ServeHTTP(w, r2)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// audit counts e and hands it to Audit.
func (s *Server) audit(e Event) {
	if s.Counts != nil {
		s.Counts.Add(e)
	}
	if s.Audit != nil {
		s.Audit(e)
	}
}
