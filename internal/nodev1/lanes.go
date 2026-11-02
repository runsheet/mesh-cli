package nodev1

import (
	"context"
	"fmt"
	"path"
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
//   - with its own copy of the lease's services (the checkout's
//     .mesh/services.yml and the brief's pods), so every shard has a
//     Postgres of its own on 127.0.0.1, as the lease's pod has, and a
//     required one that is not ready fails the lane, never a shard run
//     without its database;
//   - on the lease's resources (the brief's CPUs and memory), held of
//     the node's scheduler as any lease's are, so a lane waits for room
//     on a busy node rather than over-commit it;
//   - with no network: the lease's relay is a sidecar of the lease's
//     own pod (or a bridge in its agent's container), which a lane
//     cannot reach, so a lane reaches its own services and the node's
//     package stores and nothing else;
//   - named <lease pod>-lane-<n> and labelled as the lease's pod, so the
//     supervisor's reconcile removes one a dead node left behind; and
//     removed, its scheduler hold given back, when its steps have run.

// laneSteps is podSteps that can open a lane (validate.LaneRunner).
type laneSteps struct{ podSteps }

// Lane implements validate.LaneRunner.
func (s laneSteps) Lane(ctx context.Context, name string) (validate.Runner, func(), error) {
	lp, release, err := s.p.lanes(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	return podSteps{p: lp, dir: s.dir, env: s.env}, release, nil
}

// laneRenewEvery is how often a lane renews its scheduler hold.
const laneRenewEvery = 30 * time.Second

// suiteLanes opens lanes beside parent, the lease's pod: nil on a node
// without the sandbox's pods.
func (r *Runtime) suiteLanes(b *meshapi.Brief, workspace string, parent *leasePod) func(context.Context, string) (*leasePod, func(), error) {
	if r.Pods == nil {
		return nil
	}
	var n atomic.Int64
	return func(ctx context.Context, name string) (*leasePod, func(), error) {
		k := n.Add(1)
		hold, err := r.holdLane(ctx, b, k)
		if err != nil {
			return nil, nil, fmt.Errorf("waiting for the node's room for lane %s: %w", name, err)
		}
		sb := r.Pods.WithReserve(nil) // the scheduler holds the lane's CPUs
		sb.TempDir = r.leaseDir(b.LeaseID)
		sb.Config.Network = sandbox.Isolated()
		gn, ge := podIdentity(b, r.Node.Name)
		sess, err := sb.Open(fmt.Sprintf("%s-lane-%d", b.LeaseID, k), b.Kind, sandbox.GitIdentity{Name: gn, Email: ge})
		if err != nil {
			hold.release()
			return nil, nil, err
		}
		lp := &leasePod{sess: sess, name: fmt.Sprintf("%s-lane-%d", parent.name, k), denied: parent.denied}
		sess.Pin(lp.name, map[string]string{PodLeaseLabel: b.LeaseID, PodJobLabel: b.JobID, PodNodeLabel: r.NodeID()})
		sess.SetLimits(limitsOf(b.Resources))
		start := time.Now()
		release := func() {
			if err := sess.Close(); err != nil {
				r.logf("nodev1: lease %s: %s", b.LeaseID, sandbox.CloseLine(lp.name, err))
			}
			hold.release()
		}
		if err := r.startServices(ctx, b, lp, workspace, "the lane "+name+"'s"); err != nil {
			release()
			return nil, nil, err
		}
		r.logf("nodev1: lease %s: run_suite's lane %s runs in pod %s (opened in %s)", b.LeaseID, name, lp.name, time.Since(start).Round(time.Second))
		return lp, release, nil
	}
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

// holdLane takes the lease's resources again for lane k, waiting for
// room as long as ctx allows: a lane is scheduled against the node's
// capacity as any exec is.
func (r *Runtime) holdLane(ctx context.Context, b *meshapi.Brief, k int64) (*laneHold, error) {
	if r.Scheduler == nil {
		return &laneHold{}, nil
	}
	ask := scheduler.Ask{Owner: fmt.Sprintf("%s/lane-%d", b.LeaseID, k), CPUs: b.Resources.CPUs, MemGB: b.Resources.MemGB,
		Cache: scheduler.CacheShared, TTL: 3 * laneRenewEvery}
	l, err := r.Scheduler.Take(ctx, ask)
	if err != nil {
		return nil, err
	}
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
	return h, nil
}

// validationKind says whether the brief's kind is a validation, which
// is held to the repository's validate.yml budget (ADR-049): the kind
// named validate, bare or under its product.
func validationKind(b *meshapi.Brief) bool {
	return path.Base(b.Kind) == "validate"
}
