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
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
	"projectmesh.io/mesh-cli/internal/sigstore"
)

// Runtime is one v1 node.
type Runtime struct {
	// API is Mesh, on the node's machine token.
	API *meshapi.Client
	// Node is what the node registers as; OS and Arch default to this
	// build's.
	Node meshapi.RegisterRequest
	// Connect are the (tenant, product) pairs the node offers itself to
	// once registered (runsheet ADR 066): Mesh offers a node only the
	// jobs of a product connected to it for the job's tenant, and accepts
	// an offer on creation when the keeper is the estate admin or owns the
	// tenant. An offer Mesh leaves waiting is logged, not fatal.
	Connect []meshapi.ConnectRequest
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
	// LatchkeyHTTP reaches Latchkey for a lease's session tokens and git
	// tokens; a plain client when nil.
	LatchkeyHTTP *http.Client
	// GitUpstream, when set, is the git host every lease's relay reaches
	// whatever its brief names — a test's fake vendor; else each lease's
	// grant reaches its brief's vendor and base.
	GitUpstream string
	// SigningKey is the node's ed25519 key, whose public half it
	// registers: a node tool's record is signed with it when Fulcio or
	// Rekor cannot be used.
	SigningKey ed25519.PrivateKey
	// Sigstore is the node's Fulcio and Rekor: a node tool's record is
	// signed keylessly through them as the lease's session. Nil signs
	// every record with SigningKey, marked node-signed.
	Sigstore *sigstore.Signer
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
	relay  *gitrelay.Server
	mu     sync.Mutex
	live   map[string]*running
	access map[string][]meshapi.AccessRecord
	drain  bool
}

// running is one lease in flight.
type running struct {
	cancel     context.CancelCauseFunc
	progressAt time.Time
	// cancelled is why Mesh cancelled the lease, once it has
	cancelled string
	// mailbox reaches the lease's live session; turnTool is the brief's,
	// to read a turn with.
	mailbox  *Mailbox
	turnTool string
}

// touch is the lease's session showing progress: its progress_at in the
// next heartbeat (Mesh v1 spec: the output stream feeds liveness).
func (r *Runtime) touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.live[id]; l != nil {
		l.progressAt = time.Now()
	}
}

// deliver leaves msg in the lease's mailbox; a lease the node does not
// hold has none.
func (r *Runtime) deliver(id string, msg Message) error {
	r.mu.Lock()
	l := r.live[id]
	r.mu.Unlock()
	if l == nil || l.mailbox == nil {
		return fmt.Errorf("the node holds no lease %s", id)
	}
	return l.mailbox.Post(msg)
}

// errLeaseGone ends a lease's work with nothing more to report: Mesh
// no longer holds the lease as this node's. errCancelled is Mesh asking
// the node to stop — over its event stream or in a heartbeat's answer —
// and the node hands the lease back with cause cancelled, which ends
// the job cancelled (a lease already gone answers 409, and that is all).
var (
	errLeaseGone = errors.New("the lease is no longer this node's")
	errCancelled = errors.New("Mesh cancelled the lease")
)

// cancelLease stops the lease's work for Mesh's cancel; a lease the node
// does not hold is nothing to stop.
func (r *Runtime) cancelLease(id, why string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.live[id]
	if l == nil {
		return false
	}
	if l.cancelled == "" {
		l.cancelled = why
	}
	l.cancel(errCancelled)
	return true
}

// cancelReason is why Mesh cancelled the lease.
func (r *Runtime) cancelReason(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.live[id]; l != nil && l.cancelled != "" {
		return l.cancelled
	}
	return "Mesh cancelled the lease"
}

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
	r.relay = &gitrelay.Server{Upstream: r.GitUpstream, Audit: r.audit}
	if err := r.relay.Start(ctx); err != nil {
		return fmt.Errorf("nodev1: the git relay: %w", err)
	}
	node, err := r.API.Register(ctx, r.Node)
	if err != nil {
		return fmt.Errorf("nodev1: register: %w", err)
	}
	r.mu.Lock()
	r.nodeID = node.NodeID
	r.live = map[string]*running{}
	r.access = map[string][]meshapi.AccessRecord{}
	r.mu.Unlock()
	r.logf("nodev1: registered as %s (%s)", node.NodeID, r.Node.Name)
	if err := r.connect(ctx, node.NodeID); err != nil {
		return err
	}

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
	go r.events(hbCtx)

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

// connect offers the node to each of Connect.
func (r *Runtime) connect(ctx context.Context, nodeID string) error {
	for _, want := range r.Connect {
		c, err := r.API.Connect(ctx, nodeID, want)
		if err != nil {
			return fmt.Errorf("nodev1: connect to %s for %s: %w", want.Tenant, want.Product, err)
		}
		if c.Status == "accepted" {
			r.logf("nodev1: connected to %s for %s (%s)", c.Tenant, c.Product, c.Via)
			continue
		}
		r.logf("nodev1: the connection to %s for %s is %s: it runs nothing until the tenant accepts it", c.Tenant, c.Product, c.Status)
	}
	return nil
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
	if resp.Drain {
		r.mu.Lock()
		r.drain = true
		r.mu.Unlock()
	}
	for _, id := range resp.Cancel {
		r.cancelLease(id, "Mesh cancelled the lease (heartbeat)")
	}
}

// audit keeps one relay event as the lease's AccessRecord, posted to
// Mesh when the lease ends.
func (r *Runtime) audit(e gitrelay.Event) {
	if e.Lease == "" {
		return
	}
	rec := meshapi.AccessRecord{Kind: "git", Vendor: e.Vendor, Repo: e.Repo, Service: e.Service, Refs: e.Refs, Outcome: e.Outcome, Reason: e.Reason, Bytes: e.Bytes, At: e.At.UTC()}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.access != nil {
		r.access[e.Lease] = append(r.access[e.Lease], rec)
	}
}

// auditAPI keeps one vendor API call the lease's relay answered as an
// AccessRecord of kind api, posted to Mesh when the lease ends.
func (r *Runtime) auditAPI(lease string, c sessionproxy.APICall) {
	rec := meshapi.AccessRecord{Kind: "api", Vendor: c.Vendor, Method: c.Method, Path: c.Path, Outcome: c.Outcome, Reason: c.Reason, Bytes: c.Bytes, At: c.At.UTC()}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.access != nil {
		r.access[lease] = append(r.access[lease], rec)
	}
}

// takeAccess answers and forgets the lease's AccessRecords.
func (r *Runtime) takeAccess(lease string) []meshapi.AccessRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.access[lease]
	delete(r.access, lease)
	return out
}

// turnTool is the turn tool of the lease's brief.
func (r *Runtime) turnTool(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.live[id]; l != nil {
		return l.turnTool
	}
	return ""
}
