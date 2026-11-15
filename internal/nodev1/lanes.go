package nodev1

import (
	"context"
	"fmt"
	"io"
	"math"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/validate"
)

// A suite's shards and groups run at once (ADR-049; ADR-013, amended
// 2026-10-28; mesh-specs gap 36). run_suite runs validate.yml's steps
// through validate.Run, which runs each group, and each shard of a step
// declaring shards, in a lane of its own when its runner can open one
// (validate.LaneRunner). A sandboxed lease's runner can: the first lane
// runs in the lease's own pod, beside its services, and every other in
// a pod of its own beside the lease's —
//
//   - seeing what a step in the lease's pod sees (ADR-049 amendment, a
//     lane sees its lease; 2026-11-02, every runsheet shard but the
//     first failed at once, 0 tests, on a lane's empty HOME with no
//     network: "go: ... dial tcp: lookup proxy.golang.org ... network is
//     unreachable", [setup failed]): the same checkout, the lease's own
//     HOME (sandbox.OpenBeside) — Go's module and build caches, ~/go/bin,
//     the tools and files a step or the session put there, the relay's
//     CA — the same image, environment and PATH, and the lease's
//     proxyEnv and registryEnv;
//   - with its own copy of the lease's services (the checkout's
//     .mesh/services.yml and the brief's pods), so every shard has a
//     Postgres of its own on 127.0.0.1, as the lease's pod has, and a
//     required one that is not ready fails the lane, never a shard run
//     without its database;
//   - on the lease's warm cache (ADR-044 amendment, a lane starts from
//     the lease's warm cache): the tenant's GOMODCACHE, npm and pnpm
//     overlays at the paths the lease's pod mounts them, each with an
//     upper and work dir of the lane's own under the lease's directory,
//     the node's shared GOCACHE, and the same variables naming them
//     (laneCacheMounts);
//   - on the room its suite asks for (validate.yml's cpus and memory;
//     ADR-049 amendment, a lane takes the room its suite asks for), the
//     node's sandbox default when it names none, never the lease's
//     allowance: its steps limited to it and the same held of the
//     node's scheduler as any lease's is, so a lane waits for room on a
//     busy node rather than over-commit it (laneRoom);
//   - on the lease's network and through the lease's egress, never a
//     raw network of its own: a lease whose relay is its pod's only exit
//     (a sidecar, or a kind's egress) has lanes with no network but
//     their loopback, where each step's own bridge (gitBridgeScript)
//     pipes to the lease's relay on a socket mounted into the lane —
//     with a relay sidecar, the node's lane socket (relaySidecar.
//     openLaneTunnel), which serves the lease's egress tunnel alone
//     (CONNECT on its policy, its interception and its audit) and none
//     of the sidecar's credentials or routes; otherwise the lease's
//     sessionproxy socket, which the lease's pod reaches the same way.
//     A lease whose pod has the public network gives its lanes the same;
//   - named <lease pod>-lane-<n> and labelled as the lease's pod, so the
//     supervisor's reconcile removes one a dead node left behind; and
//     removed, its scheduler hold given back, when its steps have run;
//     the HOME stays, the lease's;
//   - and a step that fails in it, or in the lease's own pod, is in the
//     node's log: the lane, the step's exit and the last lines it wrote
//     (logFailedStep).

// laneSteps is podSteps that can open a lane (validate.LaneRunner) and
// give back the first lane's hold once it has run (validate.FirstLaneReleaser).
// sizes is the room each lane of the run asks for, by its name
// (validate.LaneSizes); first is the first lane, which runs in the
// lease's own pod.
type laneSteps struct {
	podSteps
	sizes map[string]validate.Resources
	first *firstLane
}

// newLaneSteps is run_suite's runner over steps in p, the lease's pod.
func newLaneSteps(p *leasePod, steps []validate.Step, base podSteps) laneSteps {
	ls := laneSteps{podSteps: base, sizes: map[string]validate.Resources{}, first: &firstLane{p: p}}
	for i, l := range validate.LaneSizes(steps) {
		ls.sizes[l.Name] = l.Resources
		if i == 0 {
			ls.first.name, ls.first.size = l.Name, l.Resources
		}
	}
	return ls
}

// Lane implements validate.LaneRunner.
func (s laneSteps) Lane(ctx context.Context, name string) (validate.Runner, func(), error) {
	lp, release, err := s.p.lanes(ctx, name, s.sizes[name])
	if err != nil {
		return nil, nil, err
	}
	return podSteps{p: lp, dir: s.dir, env: s.env, lane: name, logf: s.logf}, release, nil
}

// Run implements validate.Runner for the first lane, in the lease's own
// pod: its steps run on the room the lane's suites ask for, held of the
// node's scheduler from the first of them (firstLane.open), not on the
// lease's allowance.
func (s laneSteps) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	limits, err := s.first.open(ctx)
	if err != nil {
		return -1, fmt.Errorf("waiting for the node's room for lane %s: %w", s.first.name, err)
	}
	ps := s.podSteps
	ps.limits = limits
	return ps.Run(ctx, cmd, timeout, stdout, stderr)
}

// ReleaseFirst implements validate.FirstLaneReleaser: the first lane runs
// in the lease's own pod, which stays up for the rest of the run, but its
// scheduler hold is given back once its steps have run, as every other
// lane's is when it is done.
func (s laneSteps) ReleaseFirst() { s.first.release() }

// firstLane is a run's first lane, the one in the lease's own pod: the
// room its suites ask for, held of the node's scheduler from its first
// step (the hold reserveLanes took for it, else one taken then) until
// ReleaseFirst or the run's end, and the limits its steps run with — the
// same room (leasePod.hold).
type firstLane struct {
	p    *leasePod
	name string
	size validate.Resources

	mu     sync.Mutex
	opened bool
	hold   *laneHold
	limits sandbox.Limits
}

// open holds the lane's room, once, and answers its steps' limits.
func (f *firstLane) open(ctx context.Context) (sandbox.Limits, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.opened || f.p.hold == nil {
		return f.limits, nil
	}
	hold, err := f.p.hold(ctx, f.name, f.size)
	if err != nil {
		return sandbox.Limits{}, err
	}
	f.opened, f.hold, f.limits = true, hold, limitsOf(hold.room)
	return f.limits, nil
}

// release gives the lane's hold back; again, or before open, is nothing.
func (f *firstLane) release() {
	f.mu.Lock()
	hold := f.hold
	f.hold = nil
	f.mu.Unlock()
	hold.release()
}

// laneRenewEvery is how often a lane renews its scheduler hold.
const laneRenewEvery = 30 * time.Second

// laneBridgeName is the lanes' bridge script in the lease's HOME: the
// git bridge's, written once per lease for every lane to run.
const laneBridgeName = "mesh-lane-bridge.js"

// leaseNetwork is the network a lease's pod, and each of its lanes, is
// on: none but its loopback when its relay is a sidecar, the relay as
// its only exit when its kind declares egress, else the sandbox's.
func (r *Runtime) leaseNetwork(b *meshapi.Brief) sandbox.NetworkPolicy {
	switch {
	case r.sidecarMode():
		return sandbox.Isolated()
	case b.Egress != nil:
		return r.Pods.Config.Network.RelayOnly()
	}
	return r.Pods.Config.Network
}

// lanesDirName is the directory, under a lease's own, of its lanes'
// own directories (laneDir).
const lanesDirName = "lanes"

// laneDir is the kth lane's own directory under its lease's: its warm
// cache overlays' upper and work dirs (laneCacheMounts). Removed when
// the lane ends, and with every other lane's when the lease does
// (openLeasePod's close).
func (r *Runtime) laneDir(leaseID string, k int64) string {
	return filepath.Join(r.leaseDir(leaseID), lanesDirName, strconv.FormatInt(k, 10))
}

// laneCacheMounts gives sess, a lane opened beside parent, the warm
// cache parent's pod mounts (leaseCacheMounts): each of its tenant cache
// overlays at the same path over the same read-only lower, with an
// upper and work dir of the lane's own under dir — two lanes, or a lane
// and its lease, never share an overlay's upper — the node's shared
// GOCACHE as it is, and the variables naming them. Read off parent's
// session, so a lease that runs without its tenant cache (the runtime
// refused the overlay: cacheMountIsOff, runAgentRetryingCache) has lanes
// without it too. An overlay whose dirs cannot be made is logged and
// left out with its variable: a cold cache, never a lane that cannot
// run.
func (r *Runtime) laneCacheMounts(b *meshapi.Brief, parent, sess *sandbox.Session, dir string) {
	left := map[string]bool{}
	for _, m := range parent.SharedMounts(tenantCacheMount) {
		// leaseCacheMounts' own layout: <lease dir>/cache/<name>/upper
		lm, err := r.overlayMount(dir, filepath.Base(filepath.Dir(m.Upper)), m.Source)
		if err != nil {
			r.logf("nodev1: lease %s: lane's tenant cache: %v", b.LeaseID, err)
			left[m.Target] = true
			continue
		}
		sess.ShareMount(tenantCacheMount, lm)
	}
	for _, m := range parent.SharedMounts(buildCacheMount) {
		sess.ShareMount(buildCacheMount, m)
	}
	var env []string
	for _, kv := range parent.ExtraEnv() {
		if _, v, _ := strings.Cut(kv, "="); !left[v] {
			env = append(env, kv)
		}
	}
	sess.SetExtraEnv(env)
}

// suiteLanes opens lanes beside parent, the lease's pod, each on the
// room its suite asks for (size): nil on a node without the sandbox's
// pods.
func (r *Runtime) suiteLanes(b *meshapi.Brief, workspace string, parent *leasePod) func(context.Context, string, validate.Resources) (*leasePod, func(), error) {
	if r.Pods == nil {
		return nil
	}
	var n atomic.Int64
	var bridgeOnce sync.Once
	var bridge string
	var bridgeErr error
	return func(ctx context.Context, name string, size validate.Resources) (*leasePod, func(), error) {
		k := n.Add(1)
		hold, err := r.holdLane(ctx, b, parent, name, size, false)
		if err != nil {
			return nil, nil, fmt.Errorf("waiting for the node's room for lane %s: %w", name, err)
		}
		sb := r.Pods.WithReserve(nil) // the scheduler holds the lane's CPUs
		sb.TempDir = r.leaseDir(b.LeaseID)
		sb.Config.Network = r.leaseNetwork(b)
		// the lease's HOME, image and socket dirs: what a step in the
		// lease's pod sees (sandbox.OpenBeside)
		sess, err := sb.OpenBeside(parent.sess, fmt.Sprintf("%s-lane-%d", b.LeaseID, k))
		if err != nil {
			hold.release()
			return nil, nil, err
		}
		lp := &leasePod{sess: sess, name: fmt.Sprintf("%s-lane-%d", parent.name, k), denied: parent.denied, lane: true,
			gitPort: parent.gitPort, proxyEnv: slices.Clone(parent.proxyEnv), registryEnv: slices.Clone(parent.registryEnv)}
		sess.Pin(lp.name, map[string]string{PodLeaseLabel: b.LeaseID, PodJobLabel: b.JobID, PodNodeLabel: r.NodeID()})
		// the room the lane's suite asks for, the same its hold holds
		sess.SetLimits(limitsOf(hold.room))
		// the lease's warm cache, on overlay dirs of the lane's own
		dir := r.laneDir(b.LeaseID, k)
		r.laneCacheMounts(b, parent.sess, sess, dir)
		start := time.Now()
		release := func() {
			if err := sess.Close(); err != nil {
				r.logf("nodev1: lease %s: %s", b.LeaseID, sandbox.CloseLine(lp.name, err))
			}
			r.removeLaneDirs(b.LeaseID, dir)
			hold.release()
		}
		// the lease's egress: its relay's socket mounted, each step
		// run behind a bridge to it on the lane's loopback, where the
		// lease's proxyEnv points
		if sock := parent.laneSocket(); sock != "" && (lp.proxyEnv != nil || lp.registryEnv != nil) {
			bridgeOnce.Do(func() { bridge, bridgeErr = sess.WriteFile(laneBridgeName, gitBridgeScript()) })
			if bridgeErr != nil {
				release()
				return nil, nil, fmt.Errorf("the lane's bridge: %w", bridgeErr)
			}
			sess.ShareProxyDir(filepath.Dir(sock))
			lp.bridge = []string{"node", bridge, sock, strconv.Itoa(lp.gitPort), "--"}
		} else if lp.proxyEnv != nil {
			r.logf("nodev1: lease %s: lane %s has no way to the lease's relay; its steps reach nothing beyond the pod", b.LeaseID, name)
		}
		if err := r.startServices(ctx, b, lp, workspace, "the lane "+name+"'s"); err != nil {
			release()
			return nil, nil, err
		}
		r.logf("nodev1: lease %s: run_suite's lane %s runs in pod %s (opened in %s; %s; HOME %s, egress %s)", b.LeaseID, name, lp.name,
			time.Since(start).Round(time.Second), roomLine(hold.room), sess.Home, laneEgressLine(lp, sb.Config.Network))
		return lp, release, nil
	}
}

// laneEgressLine says how a lane reaches out, for the log.
func laneEgressLine(lp *leasePod, net sandbox.NetworkPolicy) string {
	switch {
	case lp.bridge != nil:
		return "the lease's relay at " + lp.bridge[2]
	case net.None || net.NoDNS:
		return "none"
	}
	return "the pod's network"
}

// removeLaneDirs removes dir, a lane's own directory or the lease's
// directory of them all, whatever a pod wrote there: an overlay's upper
// holds Go's module cache, read-only directories a plain RemoveAll
// leaves (sandbox.RemoveTree).
func (r *Runtime) removeLaneDirs(leaseID, dir string) {
	if r.Pods == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.Pods.RemoveTree(ctx, dir); err != nil {
		r.logf("nodev1: lease %s: removing %s: %v", leaseID, dir, err)
	}
}

// laneHold is a lane's hold on the node's scheduler, renewed until
// released, and the room it holds, which the lane's steps are limited
// to; on a node without a scheduler it holds nothing and only names the
// room.
type laneHold struct {
	room meshapi.Resources
	l    *scheduler.Lease
	stop chan struct{}
	once sync.Once
}

func (h *laneHold) release() {
	if h == nil || h.l == nil {
		return
	}
	h.once.Do(func() {
		close(h.stop)
		h.l.Release()
	})
}

// gb is bytes in GB, a scheduler's memory unit.
func gb(bytes int64) float64 { return float64(bytes) / (1 << 30) }

// roomLine is room for the log.
func roomLine(room meshapi.Resources) string {
	return fmt.Sprintf("%g cpus, %g GB", room.CPUs, math.Round(room.MemGB*100)/100)
}

// laneRoom is the room lane name of b's lease runs on: what its suite
// asks for (validate.yml's cpus and memory, each on its own), else the
// node's sandbox default (node.yaml's sandbox.cpus and sandbox.memory,
// read live: sandbox.AgentLimits) capped at the lease's own allowance. An ask the node could never fit beside the lease
// itself (its scheduler's total less the lease's own hold) is lowered
// to that, with a line in the log, rather than wait for room that
// cannot come. It is both what the lane holds of the scheduler
// (laneAsk) and what its steps are limited to (limitsOf).
func (r *Runtime) laneRoom(b *meshapi.Brief, name string, want validate.Resources) meshapi.Resources {
	def := r.Pods.AgentLimits()
	room := meshapi.Resources{CPUs: want.CPUs, MemGB: gb(want.MemoryBytes())}
	// A suite that names no size gets the sandbox default, but never more
	// than its lease's own allowance: a validate lease's unsized lanes stay
	// at the kind's 4 CPUs, as they were, instead of dl360's 8, which
	// reserved the whole node for repos that had declared nothing
	// (2026-11-16). Only a suite that asks gets more than its lease.
	if room.CPUs <= 0 {
		room.CPUs = def.CPUs
		if b.Resources.CPUs > 0 && b.Resources.CPUs < room.CPUs {
			room.CPUs = b.Resources.CPUs
		}
	}
	if room.MemGB <= 0 {
		room.MemGB = gb(validate.MemoryBytes(def.Memory))
		if b.Resources.MemGB > 0 && b.Resources.MemGB < room.MemGB {
			room.MemGB = b.Resources.MemGB
		}
	}
	if r.Scheduler == nil {
		return room
	}
	total := r.Scheduler.Total()
	if most := total.CPUs - b.Resources.CPUs; total.CPUs > 0 && most > 0 && room.CPUs > most {
		r.logf("nodev1: lease %s: lane %s asks for %g cpus, more than the node's %g beside the lease's %g; it gets %g",
			b.LeaseID, name, room.CPUs, total.CPUs, b.Resources.CPUs, most)
		room.CPUs = most
	}
	if most := total.MemGB - b.Resources.MemGB; total.MemGB > 0 && most > 0 && room.MemGB > most {
		r.logf("nodev1: lease %s: lane %s asks for %g GB, more than the node's %g beside the lease's %g; it gets %g",
			b.LeaseID, name, room.MemGB, total.MemGB, b.Resources.MemGB, most)
		room.MemGB = most
	}
	return room
}

// laneAsk is what lane name of a lease holds of the node's scheduler:
// its room (laneRoom), named so Scheduler.Leases() shows it apart from
// the lease's own pod-level hold (askOf).
func laneAsk(b *meshapi.Brief, name string, room meshapi.Resources) scheduler.Ask {
	return scheduler.Ask{Owner: b.LeaseID + "/lane-" + name, CPUs: room.CPUs, MemGB: room.MemGB,
		Cache: scheduler.CacheShared, TTL: 3 * laneRenewEvery}
}

// wrapHold starts a laneHold's renewal, so it is not reaped by its TTL
// while the lane it is held for runs.
func wrapHold(l *scheduler.Lease, room meshapi.Resources) *laneHold {
	h := &laneHold{room: room, l: l, stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(laneRenewEvery)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				l.Renew()
			}
		}
	}()
	return h
}

// holdLane holds lane name's room (laneRoom of size) of the node's
// scheduler, waiting for it as long as ctx allows: a lane is scheduled
// against the node's capacity as any exec is. reserveLanes tries this
// room eagerly, as soon as the lanes are known, and that hold is the
// one answered when it holds the room the lane asks for now; one that
// does not (the validate.yml changed since) is given back and the room
// taken afresh, so a lane's hold is always what its steps are limited
// to.
//
// first is the run's first lane, in the lease's own pod, beside an
// agent that waits on it. With no room for it right now it does not
// wait when what it asks for is within the lease's own allowance: it
// runs under the lease's hold, limited to its room, as every first lane
// did before lanes had sizes — a node whose every lease waited on room
// only another lease's end could free would never move. A first lane
// asking for more than its lease has waits for it, as any lane does.
func (r *Runtime) holdLane(ctx context.Context, b *meshapi.Brief, p *leasePod, name string, size validate.Resources, first bool) (*laneHold, error) {
	room := r.laneRoom(b, name, size)
	if h := p.popEagerLane(name); h != nil {
		if h.room == room {
			return h, nil
		}
		h.release()
	}
	if r.Scheduler == nil {
		return &laneHold{room: room}, nil
	}
	ask := laneAsk(b, name, room)
	if first {
		if l, err := r.Scheduler.TryTake(ask); err == nil {
			return wrapHold(l, room), nil
		}
		if within(room, b.Resources) {
			r.logf("nodev1: lease %s: no room for lane %s's %s right now; it runs in the lease's pod under the lease's own hold", b.LeaseID, name, roomLine(room))
			return &laneHold{room: room}, nil
		}
	}
	l, err := r.Scheduler.Take(ctx, ask)
	if err != nil {
		return nil, err
	}
	return wrapHold(l, room), nil
}

// within says whether room is no more than a lease's allowance; an
// allowance of 0 is no limit.
func within(room, lease meshapi.Resources) bool {
	return (lease.CPUs <= 0 || room.CPUs <= lease.CPUs) && (lease.MemGB <= 0 || room.MemGB <= lease.MemGB)
}

// reserveLanes eagerly holds the node's scheduler room for every lane a
// validation's suite will run in, as soon as its lanes are known —
// right after the lease's pod's services are up, before any lane is
// opened (ADR-049 amendment, a lease's lanes reserve their room as soon
// as the lane count is known) — each the room its own suite asks for
// (laneRoom), by the lane's name: the first lane is the lease's own
// pod, which runs with no hold of its own otherwise; the others are the
// pods suiteLanes opens beside it. Each is tried with TryTake, never
// blocking: a lane TryTake could not take is left for holdLane's
// blocking wait when (if) that lane actually opens, so a lease's own
// take never hangs on room its lanes do not yet need. Held on p, where
// holdLane takes them back and the lease's end gives back any no run
// took (releaseEagerLanes).
func (r *Runtime) reserveLanes(b *meshapi.Brief, workspace string, p *leasePod) {
	if r.Scheduler == nil || r.Pods == nil || !validationKind(b) {
		return
	}
	cfg, err := validate.Load(workspace)
	if err != nil {
		return
	}
	sizes := validate.LaneSizes(cfg.Steps)
	if len(sizes) <= 1 {
		return
	}
	for _, lane := range sizes {
		room := r.laneRoom(b, lane.Name, lane.Resources)
		l, err := r.Scheduler.TryTake(laneAsk(b, lane.Name, room))
		if err != nil {
			continue
		}
		p.setEagerLane(lane.Name, wrapHold(l, room))
	}
}

// validationKind says whether the brief's kind is a validation, which
// is held to the repository's validate.yml budget (ADR-049): the kind
// named validate, bare or under its product.
func validationKind(b *meshapi.Brief) bool {
	return path.Base(b.Kind) == "validate"
}
