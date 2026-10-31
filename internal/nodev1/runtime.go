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
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/githarden"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/resources"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/secreport"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
	"projectmesh.io/mesh-cli/internal/sigstore"
	"projectmesh.io/mesh-cli/internal/supervisor"
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
	// tenant. An offer Mesh leaves waiting is logged, not fatal; one Mesh
	// refuses because the product has not published (404), or answers
	// 429 or 5xx to, is offered again in the background while the node
	// runs on its other connections (connect.go).
	Connect []meshapi.ConnectRequest
	// ConnectRetry is the first wait before a refused connection is
	// offered again; each wait doubles up to ConnectRetryMax. 1 minute
	// and 5 minutes when zero.
	ConnectRetry, ConnectRetryMax time.Duration
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
	// StopRefusals is how many times a one-shot session's Stop hook
	// refuses its end while its finish tool is owed (stophook.go);
	// DefaultStopRefusals when zero.
	StopRefusals int
	// Session runs each lease's session.
	Session Session
	// LatchkeyHTTP reaches Latchkey for a lease's session tokens and git
	// tokens; a plain client when nil.
	LatchkeyHTTP *http.Client
	// GitUpstream, when set, is the git host every lease's relay reaches
	// whatever its brief names — a test's fake vendor; else each lease's
	// grant reaches its brief's vendor and base.
	GitUpstream string
	// ModelUpstreams, when set, are the origins a lease's model relay
	// reaches by provider (anthropic, openai) — a test's fake model; else
	// $MESH_MODEL_UPSTREAM_<PROVIDER>, else each provider's API.
	ModelUpstreams map[string]string
	// SecurityEvery is how often a lease's security reports are sent;
	// secreport.DefaultEvery when zero (security.go).
	SecurityEvery time.Duration
	// RegistryHTTP, when set, is how the in-process relay reaches a
	// lease's registry — a test's fake registry; else a plain client.
	RegistryHTTP *http.Client
	// EgressDial, when set, is how a lease's egress tunnel reaches an
	// allowed host — a test's fake host; else the host's public address.
	EgressDial func(ctx context.Context, network, addr string) (net.Conn, error)
	// EgressUpstream, when set, carries the requests a lease's tunnel
	// terminates (intercept.go) to their host — a test's, trusting its
	// fake registry; else the tunnel's own, on the system roots.
	EgressUpstream http.RoundTripper
	// SigningKey is the node's ed25519 key, whose public half it
	// registers: a node tool's record is signed with it when Fulcio or
	// Rekor cannot be used.
	SigningKey ed25519.PrivateKey
	// Sigstore is the node's Fulcio and Rekor: a node tool's record is
	// signed keylessly through them as the lease's session. Nil signs
	// every record with SigningKey, marked node-signed.
	Sigstore *sigstore.Signer
	// NoMirrors clones each lease's repository whole, keeping no mirror
	// of it between leases (wave 2.8's per-tenant mirrors are on by
	// default, under WorkRoot/mirrors).
	NoMirrors bool
	// MirrorsUnusedFor is how long a mirror no lease uses is kept; 14
	// days when zero.
	MirrorsUnusedFor time.Duration
	// Once stops the runtime after its first lease ends.
	Once bool
	// Logf hears the runtime's lines; nothing is logged when nil.
	Logf func(format string, args ...any)
	// HeartbeatEvery, when set, is the heartbeat's period whatever Mesh
	// names; else Mesh's heartbeat_every_s, else 30 s.
	HeartbeatEvery time.Duration
	// Backoff is the wait after a failed take; 5 s when zero.
	Backoff time.Duration
	// Scheduler, when set, owns the node's capacity (ADR-044, PR 3.1;
	// `--actors-scheduler`): each lease holds its resources there, the
	// node reports what is free on every take and runs its leases side
	// by side, a slot each. Nil runs one lease at a time.
	Scheduler *scheduler.Scheduler
	// Actors runs each lease as a job actor (ADR-044, PR 3.2; `--actors`):
	// it owns everything the lease uses, writes actor.json before each
	// state change, recovers a panic into its release, and hands back
	// pod_killed a session or pod killed from outside. At start the node
	// releases what actors a stopped node left behind.
	Actors bool
	// Sandbox, when set, is where a job actor makes its lease's pod for
	// the brief's pods (each a sidecar) and watches its agent; PodImage
	// is the image its network rules load from.
	Sandbox  sandbox.Runtime
	PodImage string
	// HostSuites is the explicit opt-in to run_suite running its steps
	// on the host when the lease has no pod: only for a node whose
	// sessions themselves run on the host, unsandboxed (dev, test, a node
	// without --sandbox). A node with a Sandbox never does, whatever this
	// says; the zero value never does (podsuites.go).
	HostSuites bool
	// Pods, when set, runs each lease's session and its services in a
	// rootless pod of the lease's own (`--sandbox`, podsession.go): the
	// checkout's .mesh/services.yml and the brief's pods as sidecars, the
	// agent on the brief's CPUs and memory, the relay reached only
	// through the lease's socket. Its Runtime is the node's Sandbox.
	Pods *sandbox.Sandbox
	// RelaySidecar, with Pods, runs each lease's relay as a sidecar in
	// its pod on the lease's credentials alone, the pod with no network
	// but its loopback (relaypod.go; `--relay-sidecar`). Off, the relay
	// is the node's own, as before.
	RelaySidecar bool
	// PodWatchEvery is how often an actor reads its pod's agent; 5 s
	// when zero.
	PodWatchEvery time.Duration
	// SupervisorMode runs the supervisor over the actors (ADR-044, PR
	// 3.3; `--supervisor`): warn logs quiet sessions and what the
	// reconcile finds, cancel escalates a quiet session, then stops it
	// stalled, and removes what no actor holds. Off, or no Actors, runs
	// none. QuietAfter is how long a session may be quiet (15 minutes
	// when zero); SuperviseEvery and ReconcileEvery are how often it
	// looks (15 s) and reconciles (a minute).
	SupervisorMode supervisor.Mode
	// CacheTrim, with Scheduler, trims the node's Go build cache off the
	// take loop, holding it exclusively (PR 3.4; `--cache-trim`); its
	// Scheduler is the runtime's.
	CacheTrim *resources.CacheTrim
	// Updater, when set, keeps the node on the newest release (PR 3.4;
	// `--self-update`): every UpdateEvery (10 minutes) it looks, and a
	// newer release drains the node, waits DrainWait (30 minutes) for
	// its leases, hands back node_update what still runs, installs, and
	// ends Run with ErrRestartForUpdate.
	Updater        resources.Updater
	UpdateEvery    time.Duration
	DrainWait      time.Duration
	QuietAfter     time.Duration
	SuperviseEvery time.Duration
	ReconcileEvery time.Duration

	// gitBridgePort, set, is a lease pod's git bridge port in place of
	// podGitPort: tests, whose fake pods share the host's loopback.
	gitBridgePort int

	nodeID  string
	relay   *gitrelay.Server
	mirrors *resources.Mirrors
	mu      sync.Mutex
	// leases are the leases in flight, each its own record (the job
	// actor's, under --actors): the node keeps no other in-flight state
	// of a lease (ADR-044; guard_test.go)
	leases actor.Registry[*running]
	// accounts are the model accounts' latest standing, from their
	// sessions' rate-limit readings, for the heartbeat (accounts.go)
	accounts map[string]*accountReading
	drain    bool
	// updating is a drain the self-update asked for; updated closes when
	// it ends, holding the version it installed
	updating bool
	updated  chan string
}

// running is one lease in flight.
type running struct {
	cancel     context.CancelCauseFunc
	progressAt time.Time
	// cancelled is why Mesh cancelled the lease, once it has
	cancelled string
	// mailbox reaches the lease's live session; turnTool is the brief's
	// turn tool as the session names it (turnToolFor), to read a turn
	// with.
	mailbox  *Mailbox
	turnTool string
	// jobID is the lease's job, for the thread sweep (thread.go).
	jobID string
	// actor is the lease's job actor, under --actors; pod its pod, and
	// escalatedAt when the supervisor last escalated it for quiet.
	actor       *actor.Actor
	pod         string
	escalatedAt time.Time
	// access are the relay's AccessRecords of the lease, posted to Mesh
	// when it ends.
	access []meshapi.AccessRecord
	// security reports what the lease's relay saw (security.go). Each
	// relay hook holds it from its making, so a relay closing after the
	// record has gone still reports, until the release's flush.
	security *secreport.Reporter
}

// touch is the lease's session showing progress: its progress_at in the
// next heartbeat (Mesh v1 spec: the output stream feeds liveness).
func (r *Runtime) touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.lease(id); l != nil {
		l.progressAt = time.Now()
	}
}

// deliver leaves msg in the lease's mailbox; a lease the node does not
// hold has none.
func (r *Runtime) deliver(id string, msg Message) error {
	r.mu.Lock()
	l := r.lease(id)
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
	l := r.lease(id)
	if l == nil {
		return false
	}
	if l.cancelled == "" {
		l.cancelled = why
	}
	if l.actor != nil {
		l.actor.Cancel(why)
	}
	l.cancel(errCancelled)
	return true
}

// cancelReason is why Mesh cancelled the lease.
func (r *Runtime) cancelReason(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.lease(id); l != nil && l.cancelled != "" {
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
	if r.Pods != nil && r.Sandbox == nil {
		r.Sandbox = r.Pods.Runtime
	}
	r.relay = &gitrelay.Server{Upstream: r.GitUpstream, Audit: r.audit}
	if err := r.relay.Start(ctx); err != nil {
		return fmt.Errorf("nodev1: the git relay: %w", err)
	}
	if !r.NoMirrors {
		r.mirrors = &resources.Mirrors{Root: filepath.Join(r.WorkRoot, "mirrors"), Env: githarden.Env(nil), GitArgs: githarden.Args()}
		go r.trimMirrors()
	}
	node, err := r.API.Register(ctx, r.Node)
	if err != nil {
		return fmt.Errorf("nodev1: register: %w", err)
	}
	r.mu.Lock()
	r.nodeID = node.NodeID
	r.mu.Unlock()
	r.logf("nodev1: registered as %s (%s)", node.NodeID, r.Node.Name)
	// a refused connection offered again in the background stops with
	// the run
	connCtx, stopConnecting := context.WithCancel(ctx)
	defer stopConnecting()
	if err := r.connect(connCtx, node.NodeID); err != nil {
		return err
	}
	if r.Actors {
		r.releaseLeftovers()
	}
	// parked threads past their keep, and threads a stopped node left
	r.sweepThreads(time.Now())

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
	if s := r.supervisor(); s != nil {
		go s.Run(hbCtx)
	}
	r.startResources(hbCtx)

	var leases sync.WaitGroup
	defer leases.Wait()
	for ctx.Err() == nil {
		if r.draining() {
			leases.Wait()
			if err := r.awaitUpdate(ctx); err != nil {
				return err
			}
			r.logf("nodev1: Mesh asked the node to drain; it takes no more leases")
			return nil
		}
		r.waitForSlot(ctx)
		if ctx.Err() != nil {
			break
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
		// the lease's room is taken before the next take reads what is
		// free
		room, roomErr := r.holdCapacity(ctx, tk.Brief, tk.Lease)
		if r.Scheduler == nil || r.Once {
			r.runLease(ctx, tk, room, roomErr)
			if r.Once {
				return nil
			}
			continue
		}
		// with a scheduler the lease runs beside the others, holding its
		// slot until it ends
		leases.Add(1)
		go func() {
			defer leases.Done()
			r.runLease(ctx, tk, room, roomErr)
		}()
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

// free is what the node can take now: what its scheduler's leases leave
// free, else its whole capacity, since it runs one lease at a time.
func (r *Runtime) free() *meshapi.Capacity {
	if r.Scheduler != nil {
		return freeOf(r.Scheduler)
	}
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
// progress, and its model accounts' standing as their sessions read it
// (accounts.go); Mesh's answer cancels leases and drains the node.
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
	for _, e := range r.leases.All() {
		beats = append(beats, meshapi.LeaseBeat{LeaseID: e.ID, ProgressAt: e.Value.progressAt.UTC()})
	}
	nodeID := r.nodeID
	r.mu.Unlock()
	accounts := r.unsentAccounts()
	resp, err := r.API.Heartbeat(ctx, nodeID, meshapi.HeartbeatRequest{
		Version: r.Node.Version, CapacityTotal: r.Node.CapacityTotal, Leases: beats, Accounts: accounts,
	})
	if err != nil {
		if ctx.Err() == nil {
			r.logf("nodev1: heartbeat: %v", err)
		}
		return
	}
	r.sentAccounts(accounts)
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
	for _, p := range e.Pushed {
		rec.Pushed = append(rec.Pushed, meshapi.PushedRef{Ref: p.Ref, SHA: p.SHA})
	}
	r.keepAccess(e.Lease, rec)
}

// keepAccess keeps rec with the lease's own record; a lease the node no
// longer runs keeps nothing.
func (r *Runtime) keepAccess(id string, rec meshapi.AccessRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.lease(id); l != nil {
		l.access = append(l.access, rec)
	}
}

// lease is the lease's record in flight, nil when the node runs no such
// lease.
func (r *Runtime) lease(id string) *running {
	l, _ := r.leases.Get(id)
	return l
}

// trimMirrors removes the mirrors no lease has used for
// MirrorsUnusedFor, once, off the take loop.
func (r *Runtime) trimMirrors() {
	unused := r.MirrorsUnusedFor
	if unused <= 0 {
		unused = 14 * 24 * time.Hour
	}
	removed, err := r.mirrors.Trim(unused)
	if err != nil {
		r.logf("nodev1: trimming mirrors: %v", err)
	}
	for _, d := range removed {
		r.logf("nodev1: removed the mirror %s, unused for %s", d, unused)
	}
}

// auditAPI keeps one vendor API call the lease's relay answered as an
// AccessRecord of kind api, posted to Mesh when the lease ends.
func (r *Runtime) auditAPI(lease string, c sessionproxy.APICall) {
	rec := meshapi.AccessRecord{Kind: "api", Vendor: c.Vendor, Method: c.Method, Path: c.Path, Outcome: c.Outcome, Reason: c.Reason, Bytes: c.Bytes, At: c.At.UTC()}
	r.keepAccess(lease, rec)
}

// takeAccess answers and forgets the lease's AccessRecords.
func (r *Runtime) takeAccess(lease string) []meshapi.AccessRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.lease(lease)
	if l == nil {
		return nil
	}
	out := l.access
	l.access = nil
	return out
}

// turnTool is the turn tool of the lease's brief.
func (r *Runtime) turnTool(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.lease(id); l != nil {
		return l.turnTool
	}
	return ""
}
