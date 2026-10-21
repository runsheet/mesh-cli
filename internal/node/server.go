package node

// The node's HTTP server (ADR-039, The node's HTTP server): one
// http.Server in the node process serves every listener the node opens
// — its admin socket, ~/.mesh/node.sock, where `mesh node status` and a
// watchdog read the node live, and each session's relay socket in its
// job's mcp/ directory. A connection carries the listener it came in on,
// so a request on a session's socket reaches that session's proxy and
// nothing else: no path, header or query on it can name another
// session, and the admin endpoints are not on it. The admin socket is
// 0600 and the node's alone; nothing listens on TCP.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AdminSocketName is the admin socket under ~/.mesh.
const AdminSocketName = "node.sock"

// AdminSocket is ~/.mesh/node.sock.
func AdminSocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", AdminSocketName), nil
}

// Server is the node's one HTTP server.
type Server struct {
	// Admin answers the admin socket (AdminHandler).
	Admin http.Handler
	Logf  func(format string, args ...any)

	once sync.Once
	srv  *http.Server

	mu        sync.Mutex
	listeners map[string]net.Listener
}

// listenerKey carries a connection's handler on its context.
type listenerKey struct{}

// taggedListener hands each connection it accepts the handler its
// requests go to.
type taggedListener struct {
	net.Listener
	h http.Handler
}

type taggedConn struct {
	net.Conn
	h http.Handler
}

func (l taggedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return taggedConn{Conn: c, h: l.h}, nil
}

func (s *Server) server() *http.Server {
	s.once.Do(func() {
		s.listeners = map[string]net.Listener{}
		s.srv = &http.Server{
			ReadHeaderTimeout: 30 * time.Second,
			ConnContext: func(ctx context.Context, c net.Conn) context.Context {
				if tc, ok := c.(taggedConn); ok {
					return context.WithValue(ctx, listenerKey{}, tc.h)
				}
				return ctx
			},
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h, _ := r.Context().Value(listenerKey{}).(http.Handler)
				if h == nil {
					http.NotFound(w, r)
					return
				}
				h.ServeHTTP(w, r)
			}),
		}
	})
	return s.srv
}

// ListenAdmin serves Admin on a unix socket at path, 0600.
func (s *Server) ListenAdmin(path string) error {
	if s.Admin == nil {
		return errors.New("node server: no admin handler")
	}
	return s.Listen(path, s.Admin)
}

// Listen serves h on a unix socket at path, 0600, until Unlisten or
// Close: a session's relay socket (sessionproxy.Proxy.ListenOn) or the
// admin socket. A socket left at path by an earlier process is removed
// first; path already served here is refused.
func (s *Server) Listen(path string, h http.Handler) error {
	srv := s.server()
	s.mu.Lock()
	_, taken := s.listeners[path]
	s.mu.Unlock()
	if taken {
		return fmt.Errorf("node server: %s is already served", path)
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	s.mu.Lock()
	s.listeners[path] = ln
	s.mu.Unlock()
	go func() {
		if err := srv.Serve(taggedListener{Listener: ln, h: h}); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			s.logf("node server: serve %s: %v", path, err)
		}
	}()
	return nil
}

// Unlisten stops serving path and removes its socket; requests already
// in flight on it finish.
func (s *Server) Unlisten(path string) error {
	s.mu.Lock()
	ln := s.listeners[path]
	delete(s.listeners, path)
	s.mu.Unlock()
	if ln == nil {
		return nil
	}
	err := ln.Close()
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}

// Paths are the sockets served now.
func (s *Server) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.listeners))
	for p := range s.listeners {
		out = append(out, p)
	}
	return out
}

// Close stops every listener and the server.
func (s *Server) Close() error {
	srv := s.server()
	s.mu.Lock()
	paths := make([]string, 0, len(s.listeners))
	for p := range s.listeners {
		paths = append(paths, p)
	}
	s.mu.Unlock()
	for _, p := range paths {
		_ = s.Unlisten(p)
	}
	return srv.Close()
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
