package gitrelay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu      sync.Mutex
	ln      net.Listener
	srv     *http.Server
	entries map[string]*entry
}

type entry struct {
	relay   *Relay
	expires time.Time
	key     string
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

// Register serves policy on token until it expires and answers the base
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
	key := token.Value + "\x00" + policy.Repo + "\x00" + string(policy.Access) + "\x00" + policy.PushRef
	for c, e := range s.entries {
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
		Policy: policy, Upstream: s.Upstream, HTTP: s.HTTP, Audit: s.Audit, Now: s.Now,
		Mint: func(context.Context) (Token, error) { return tok, nil },
		// the node renews the item's token and registers the new one; a
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
	if e == nil || !e.expires.After(s.now()) {
		http.Error(w, "gitrelay: no such relay, or its token expired", http.StatusNotFound)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + path
	e.relay.ServeHTTP(w, r2)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
