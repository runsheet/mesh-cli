// Package nodev1 is the Mesh v1 node runtime, `mesh node run --api v1`
// (Mesh v1 spec, "The mesh-cli node"): it registers with Mesh, takes a
// lease, runs one harnessed session with the brief's product MCP
// servers through the lease's sessionproxy, renews the lease while the
// session runs, and finishes or hands back. It runs beside the legacy
// runtime (internal/node) and shares none of it: it speaks only
// internal/meshapi, and a guard test refuses any import of
// internal/gateway or internal/node.
//
// This is wave 0's minimal runtime: one lease at a time, a `claude -p`
// session. The job actor, scheduler and supervisor of wave 3 replace its
// loop.
package nodev1

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// Runtime is one v1 node.
type Runtime struct {
	// API is Mesh, on the node's machine token.
	API *meshapi.Client
	// Node is what the node registers as; OS and Arch default to this
	// build's.
	Node meshapi.RegisterRequest
	// WorkRoot holds each lease's directory, leases/<lease_id>.
	WorkRoot string
	// SocketRoot is where each lease's sessionproxy socket directory is
	// made: a short path, since a unix socket's is bounded. When empty,
	// $XDG_RUNTIME_DIR/mesh, else mesh-<uid> in the temporary directory,
	// else /tmp/mesh-<uid>: the first whose socket path fits
	// (leaseSocketDir).
	SocketRoot string
	// MeshBinary is this mesh executable: the session reaches each proxied
	// server through its `mcp-relay --socket`.
	MeshBinary string
	// Session runs each lease's session.
	Session Session
	// Upstream is how the lease's proxy reaches one of the brief's
	// servers; a plain Remote, no credential, when nil.
	Upstream func(ctx context.Context, b *meshapi.Brief, s meshapi.Server) sessionproxy.Upstream
	// Once stops the runtime after its first lease ends.
	Once bool
	// Logf hears the runtime's lines; nothing is logged when nil.
	Logf func(format string, args ...any)
	// HeartbeatEvery, when set, is the heartbeat's period whatever Mesh
	// names; else Mesh's heartbeat_every_s, else 30 s.
	HeartbeatEvery time.Duration
	// Backoff is the wait after a failed take; 5 s when zero.
	Backoff time.Duration

	nodeID string
	mu     sync.Mutex
	live   map[string]*running
	drain  bool
}

// running is one lease in flight.
type running struct {
	cancel     context.CancelCauseFunc
	progressAt time.Time
}

// errLeaseGone and errCancelled end a lease's work with nothing more
// to report: Mesh no longer holds the lease as this node's.
var (
	errLeaseGone = errors.New("the lease is no longer this node's")
	errCancelled = errors.New("Mesh cancelled the lease")
)

func (r *Runtime) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// NodeID is the id Mesh registered the node as, once Run has registered.
func (r *Runtime) NodeID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodeID
}

// Run registers the node and takes leases until ctx ends, Mesh drains
// the node, or — with Once — the first lease ends.
func (r *Runtime) Run(ctx context.Context) error {
	if r.API == nil || r.Session == nil {
		return errors.New("nodev1: a runtime needs Mesh's API and a session runner")
	}
	if r.Node.OS == "" {
		r.Node.OS = runtime.GOOS
	}
	if r.Node.Arch == "" {
		r.Node.Arch = runtime.GOARCH
	}
	if r.Node.Harnesses == nil {
		r.Node.Harnesses = []string{"claude-code"}
	}
	if r.Node.Tools == nil {
		r.Node.Tools = []string{}
	}
	node, err := r.API.Register(ctx, r.Node)
	if err != nil {
		return fmt.Errorf("nodev1: register: %w", err)
	}
	r.mu.Lock()
	r.nodeID = node.NodeID
	r.live = map[string]*running{}
	r.mu.Unlock()
	r.logf("nodev1: registered as %s (%s)", node.NodeID, r.Node.Name)

	every := r.HeartbeatEvery
	if every <= 0 && node.HeartbeatEveryS > 0 {
		every = time.Duration(node.HeartbeatEveryS) * time.Second
	}
	if every <= 0 {
		every = 30 * time.Second
	}
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go r.heartbeats(hbCtx, every)

	for ctx.Err() == nil {
		if r.draining() {
			r.logf("nodev1: Mesh asked the node to drain; it takes no more leases")
			return nil
		}
		tk, err := r.API.Take(ctx, meshapi.TakeRequest{
			NodeID: node.NodeID, Free: r.free(), Harnesses: r.Node.Harnesses, WaitS: meshapi.MaxWaitS,
		})
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			r.logf("nodev1: take: %v", err)
			r.sleep(ctx, r.backoff())
			continue
		}
		if tk.Lease == nil {
			continue
		}
		r.runLease(ctx, tk)
		if r.Once {
			return nil
		}
	}
	return ctx.Err()
}

func (r *Runtime) backoff() time.Duration {
	if r.Backoff > 0 {
		return r.Backoff
	}
	return 5 * time.Second
}

func (r *Runtime) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// free is what the node can take now: its whole capacity, since this
// runtime runs one lease at a time.
func (r *Runtime) free() *meshapi.Capacity {
	c := r.Node.CapacityTotal
	if c.Slots == 0 {
		c.Slots = 1
	}
	return &c
}

func (r *Runtime) draining() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drain
}

// heartbeats reports the node every period: its leases and their
// progress; Mesh's answer cancels leases and drains the node.
func (r *Runtime) heartbeats(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.heartbeat(ctx)
	}
}

func (r *Runtime) heartbeat(ctx context.Context) {
	r.mu.Lock()
	var beats []meshapi.LeaseBeat
	for id, l := range r.live {
		beats = append(beats, meshapi.LeaseBeat{LeaseID: id, ProgressAt: l.progressAt.UTC()})
	}
	nodeID := r.nodeID
	r.mu.Unlock()
	resp, err := r.API.Heartbeat(ctx, nodeID, meshapi.HeartbeatRequest{
		Version: r.Node.Version, CapacityTotal: r.Node.CapacityTotal, Leases: beats,
	})
	if err != nil {
		if ctx.Err() == nil {
			r.logf("nodev1: heartbeat: %v", err)
		}
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if resp.Drain {
		r.drain = true
	}
	for _, id := range resp.Cancel {
		if l := r.live[id]; l != nil {
			l.cancel(errCancelled)
		}
	}
}
