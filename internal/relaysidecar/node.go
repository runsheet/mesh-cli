package relaysidecar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// The lease's socket to the node. The sidecar is its one client: only
// the sidecar's container has it mounted. What the node serves on it:
//
//   - CONNECT host:port, the tunnel, which allows or refuses by host and
//     port and then pipes bytes it never reads;
//   - /model/<provider>/…, the node's model forwarder, which adds the
//     node's model credential to a fixed set of paths (the one place
//     the node reads HTTP a pod sent it, until a lease-scoped model
//     token exists);
//   - /mcp/<node tools> and /stop, the node tools and the Stop hook's
//     gate, which sign evidence with the lease's identity and so stay
//     on the node; the sidecar forwards them unchanged;
//   - RenewPath, a fresh session token and git token for the lease;
//   - AccessPath, the sidecar's AccessRecords of the lease.

// Paths of the lease's socket and of the sidecar's control socket.
const (
	RenewPath  = "/relay/renew"
	AccessPath = "/relay/access"
	StopPath   = "/stop"
	// PushPath is served by the sidecar, on its control socket, for the
	// node's push tool.
	PushPath = "/relay/push"
)

// accessMax bounds one AccessPath post.
const (
	accessMax      = 256
	accessBodyMax  = 1 << 20
	renewBodyMax   = 4 << 10
	renewTimeout   = 30 * time.Second
	controlBodyMax = 16 << 10
)

// RenewRequest asks the node for the lease's credentials again: the
// session token always, the git token when Git is set.
type RenewRequest struct {
	Git bool `json:"git,omitempty"`
}

// Renewal is the node's answer: lease-scoped tokens alone.
type Renewal struct {
	Session Token  `json:"session"`
	Git     *Token `json:"git,omitempty"`
}

// Node serves the lease's socket for its sidecar.
type Node struct {
	// Tunnel answers CONNECT: the lease's egress allowlist (internal/
	// egress). nil refuses every CONNECT.
	Tunnel http.Handler
	// Proxy serves /mcp/<server> and /model/: a sessionproxy holding
	// only the node tools (in process) and the model forwarder
	// (sessionproxy.Model with Paths). nil serves neither.
	Proxy http.Handler
	// Stop is the Stop hook's gate. nil serves none.
	Stop http.Handler
	// Renew mints the lease's tokens again. nil refuses renewal.
	Renew func(ctx context.Context, req RenewRequest) (Renewal, error)
	// Access keeps one of the lease's AccessRecords. nil drops them.
	Access func(meshapi.AccessRecord)
	// Logf takes the node's lines; never a credential.
	Logf func(format string, args ...any)

	mu  sync.Mutex
	srv *http.Server
	ln  net.Listener
}

// ServeHTTP routes one request from the sidecar.
func (n *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		if n.Tunnel == nil {
			http.Error(w, "this lease has no egress", http.StatusForbidden)
			return
		}
		n.Tunnel.ServeHTTP(w, r)
	case r.URL.Path == RenewPath:
		n.serveRenew(w, r)
	case r.URL.Path == AccessPath:
		n.serveAccess(w, r)
	case r.URL.Path == StopPath:
		if n.Stop == nil {
			http.NotFound(w, r)
			return
		}
		n.Stop.ServeHTTP(w, r)
	case strings.HasPrefix(r.URL.Path, sessionproxy.Prefix), strings.HasPrefix(r.URL.Path, sessionproxy.ModelPrefix):
		if n.Proxy == nil {
			http.NotFound(w, r)
			return
		}
		n.Proxy.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (n *Node) serveRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	if n.Renew == nil {
		http.Error(w, "this lease renews nothing", http.StatusNotFound)
		return
	}
	var req RenewRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, renewBodyMax)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "not a renewal", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), renewTimeout)
	defer cancel()
	out, err := n.Renew(ctx, req)
	if err != nil {
		n.logf("relaysidecar: renewal: %v", err)
		http.Error(w, "the node could not renew the lease's credentials", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (n *Node) serveAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	var recs []meshapi.AccessRecord
	if err := json.NewDecoder(io.LimitReader(r.Body, accessBodyMax)).Decode(&recs); err != nil {
		http.Error(w, "not AccessRecords", http.StatusBadRequest)
		return
	}
	if len(recs) > accessMax {
		http.Error(w, "too many AccessRecords", http.StatusRequestEntityTooLarge)
		return
	}
	if n.Access != nil {
		for _, rec := range recs {
			if rec.Kind != "git" && rec.Kind != "api" && rec.Kind != "mcp" {
				continue
			}
			n.Access(rec)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (n *Node) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
	}
}

// Listen serves the node's side on a unix socket at path, mode 0666 so
// the sidecar's own UID can reach it: the socket's directory is the
// gate, mounted into the sidecar's container alone.
func (n *Node) Listen(path string) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: n, ReadHeaderTimeout: 30 * time.Second}
	n.mu.Lock()
	n.srv, n.ln = srv, ln
	n.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			n.logf("relaysidecar: the lease's socket: %v", err)
		}
	}()
	return nil
}

// Close closes the socket and every connection on it, the tunnels'
// hijacked ones by the Tunnel's own Close.
func (n *Node) Close() error {
	n.mu.Lock()
	srv := n.srv
	n.srv = nil
	n.mu.Unlock()
	if srv == nil {
		return nil
	}
	if c, ok := n.Tunnel.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	return srv.Close()
}
