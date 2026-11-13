package nodev1

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
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
//   - on the lease's resources (the brief's CPUs and memory), held of
//     the node's scheduler as any lease's are, so a lane waits for room
//     on a busy node rather than over-commit it;
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
type laneSteps struct{ podSteps }

// Lane implements validate.LaneRunner.
func (s laneSteps) Lane(ctx context.Context, name string) (validate.Runner, func(), error) {
	lp, release, err := s.p.lanes(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	return podSteps{p: lp, dir: s.dir, env: s.env, lane: name, logf: s.logf}, release, nil
}

// ReleaseFirst implements validate.FirstLaneReleaser: the first lane runs
// in the lease's own pod, which stays up for the rest of the run, but its
// eagerly-reserved scheduler hold (reserveLanes) is given back once its
// steps have run, as every other lane's is when it is done.
func (s laneSteps) ReleaseFirst() {
	s.p.releaseEagerLane(1)
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

// suiteLanes opens lanes beside parent, the lease's pod: nil on a node
// without the sandbox's pods.
func (r *Runtime) suiteLanes(b *meshapi.Brief, workspace string, parent *leasePod) func(context.Context, string) (*leasePod, func(), error) {
	if r.Pods == nil {
		return nil
	}
	var n atomic.Int64
	var bridgeOnce sync.Once
	var bridge string
	var bridgeErr error
	return func(ctx context.Context, name string) (*leasePod, func(), error) {
		k := n.Add(1)
		// idx is the lane's eager reservation index (reserveLanes): 1 is
		// the first lane, the lease's own pod, which never reaches here;
		// every physical pod opened here is 2..n.
		idx := k + 1
		hold := parent.popEagerLane(idx)
		if hold == nil {
			var err error
			if hold, err = r.holdLane(ctx, b, idx); err != nil {
				return nil, nil, fmt.Errorf("waiting for the node's room for lane %s: %w", name, err)
			}
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
		sess.SetLimits(limitsOf(b.Resources))
		start := time.Now()
		release := func() {
			if err := sess.Close(); err != nil {
				r.logf("nodev1: lease %s: %s", b.LeaseID, sandbox.CloseLine(lp.name, err))
			}
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
		r.logf("nodev1: lease %s: run_suite's lane %s runs in pod %s (opened in %s; HOME %s, egress %s)", b.LeaseID, name, lp.name,
			time.Since(start).Round(time.Second), sess.Home, laneEgressLine(lp, sb.Config.Network))
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

// laneHold is a lane's hold on the node's scheduler, renewed until
// released; nothing on a node without one.
type laneHold struct {
	l    *scheduler.Lease
	stop chan struct{}
}

func (h *laneHold) release() {
	if h == nil || h.l == nil {
		return
	}
	close(h.stop)
	h.l.Release()
}

// laneAsk is what lane idx of a lease holds of the node's scheduler: the
// lease's own resources, named so Scheduler.Leases() shows it apart from
// the lease's own pod-level hold (askOf).
func laneAsk(b *meshapi.Brief, idx int64) scheduler.Ask {
	return scheduler.Ask{Owner: fmt.Sprintf("%s/lane-%d", b.LeaseID, idx), CPUs: b.Resources.CPUs, MemGB: b.Resources.MemGB,
		Cache: scheduler.CacheShared, TTL: 3 * laneRenewEvery}
}

// wrapHold starts a laneHold's renewal, so it is not reaped by its TTL
// while the lane it is held for runs.
func wrapHold(l *scheduler.Lease) *laneHold {
	h := &laneHold{l: l, stop: make(chan struct{})}
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

// holdLane takes the lease's resources again for lane idx, waiting for
// room as long as ctx allows: a lane is scheduled against the node's
// capacity as any exec is. reserveLanes tries this room eagerly, as soon
// as the lane count is known; holdLane is the fallback for an index it
// could not reserve, at the lane's actual open.
func (r *Runtime) holdLane(ctx context.Context, b *meshapi.Brief, idx int64) (*laneHold, error) {
	if r.Scheduler == nil {
		return &laneHold{}, nil
	}
	l, err := r.Scheduler.Take(ctx, laneAsk(b, idx))
	if err != nil {
		return nil, err
	}
	return wrapHold(l), nil
}

// reserveLanes eagerly holds the node's scheduler room for every lane a
// validation's suite will run in, as soon as the lane count is known —
// right after the lease's pod's services are up, before any lane is
// opened (ADR-049 amendment, a lease's lanes reserve their room as soon
// as the lane count is known): index 1 is the first lane, the lease's
// own pod, which runs with no hold of its own otherwise; indices 2..n
// are the pods suiteLanes opens beside it. Each is tried with TryTake,
// never blocking: an index TryTake could not take is left for holdLane's
// blocking wait when (if) that lane actually opens, so a lease's own
// take never hangs on room its lanes do not yet need. Held on p, where
// suiteLanes and laneSteps.ReleaseFirst take them back.
func (r *Runtime) reserveLanes(b *meshapi.Brief, workspace string, p *leasePod) {
	if r.Scheduler == nil || r.Pods == nil || !validationKind(b) {
		return
	}
	cfg, err := validate.Load(workspace)
	if err != nil {
		return
	}
	n := validate.Lanes(cfg.Steps)
	if n <= 1 {
		return
	}
	for idx := int64(1); idx <= int64(n); idx++ {
		l, err := r.Scheduler.TryTake(laneAsk(b, idx))
		if err != nil {
			continue
		}
		p.setEagerLane(idx, wrapHold(l))
	}
}

// validationKind says whether the brief's kind is a validation, which
// is held to the repository's validate.yml budget (ADR-049): the kind
// named validate, bare or under its product.
func validationKind(b *meshapi.Brief) bool {
	return path.Base(b.Kind) == "validate"
}
