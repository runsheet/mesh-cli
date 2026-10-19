package mcprelay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MaxSocketPath is the longest unix socket path bound as it is:
// sun_path is 108 bytes on Linux and 104 on macOS, the terminator
// included. A longer one is bound through its directory's descriptor
// (listenUnix).
const MaxSocketPath = 103

// Endpoint is the node's half of a pod session's runsheet relay: an
// HTTP server on a unix socket the pod reaches through its HOME's
// mount, forwarding every request to URL with the bearer read from
// TokenFile per request. The bearer stays on the node: the pod holds the
// socket's path, never the token or a file holding it (goal
// a-session-runs-as-ordered-steps-like-an-actions-job, item 3). A client
// Authorization header is replaced, never forwarded.
type Endpoint struct {
	// Socket is the unix socket's path.
	Socket string
	// URL is the server's MCP endpoint, e.g. https://api.runsheet.dev/mcp.
	URL string
	// TokenFile holds the bearer, read and trimmed on every request.
	TokenFile string

	ln    net.Listener
	srv   *http.Server
	proxy *httputil.ReverseProxy
	once  sync.Once
}

// Serve listens on socket (0600, a stale socket there removed first) and
// serves the endpoint until Close. transport is the upstream's; nil is
// http.DefaultTransport.
func Serve(socket, target, tokenFile string, transport http.RoundTripper) (*Endpoint, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("relay url %q: not an absolute URL", target)
	}
	if fi, err := os.Lstat(socket); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(socket) // a node before this one served it
	}
	ln, err := listenUnix(socket)
	if err != nil {
		return nil, fmt.Errorf("relay socket %s: %w", socket, err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		_ = os.Remove(socket)
		return nil, fmt.Errorf("relay socket: %w", err)
	}
	e := &Endpoint{Socket: socket, URL: target, TokenFile: tokenFile, ln: ln}
	e.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := *u
			pr.Out.URL = &out
			pr.Out.Host = ""
		},
		Transport: transport,
		// an event stream is relayed as it arrives
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "mesh relay endpoint: "+err.Error(), http.StatusBadGateway)
		},
	}
	e.srv = &http.Server{Handler: e}
	go func() { _ = e.srv.Serve(ln) }()
	return e, nil
}

// listenUnix listens on socket; a path past MaxSocketPath is bound on
// Linux as /proc/self/fd/<dir>/<name>, its directory held open for the
// bind alone. The listener never unlinks: Close removes the socket by
// its own path.
func listenUnix(socket string) (net.Listener, error) {
	addr := socket
	if len(socket) > MaxSocketPath {
		dir, err := os.Open(filepath.Dir(socket))
		if err != nil {
			return nil, err
		}
		defer dir.Close()
		addr = fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(socket))
		if len(addr) > MaxSocketPath {
			return nil, fmt.Errorf("path is %d bytes, past the %d a unix socket takes", len(socket), MaxSocketPath)
		}
	}
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	return ln, nil
}

// ServeHTTP forwards one request upstream on the file's bearer of the
// moment; a missing or empty file is a 502 naming it.
func (e *Endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok, err := readToken(e.TokenFile)
	if err != nil {
		http.Error(w, "mesh relay endpoint: "+err.Error(), http.StatusBadGateway)
		return
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	e.proxy.ServeHTTP(w, r)
}

// Close stops serving and removes the socket.
func (e *Endpoint) Close() error {
	var err error
	e.once.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // in-flight requests are the session's, which is over
		err = e.srv.Shutdown(ctx)
		if errors.Is(err, context.Canceled) {
			err = e.srv.Close()
		}
		_ = os.Remove(e.Socket)
	})
	return err
}

// readToken is path's contents, trimmed: a missing or empty file is an
// error naming it.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}
