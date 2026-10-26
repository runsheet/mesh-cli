package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// RuntimeBrokenPrefix opens the reason a session whose pod could not
// start on a broken container runtime hands its work back with
// (validate.RuntimeBroken, ADR-030), and the hold's entry for it.
const RuntimeBrokenPrefix = string(validate.RuntimeBroken) + ": "

// RuntimeErrorOf is the *sandbox.RuntimeError in err's chain — the
// container runtime itself unable to start the session's pod — or nil.
func RuntimeErrorOf(err error) *sandbox.RuntimeError {
	var re *sandbox.RuntimeError
	if errors.As(err, &re) {
		return re
	}
	return nil
}

// RuntimeReason is the reason a runtime failure hands the platform:
// `runtime_broken: <op>: <podman's words>`, scrubbed and cut to 2000.
func RuntimeReason(re *sandbox.RuntimeError) string {
	return truncate(RuntimeBrokenPrefix+scrubbed(fmt.Errorf("%s: %w", re.Op, re.Err), nil).Error(), 2000)
}

// ReleaseOnRuntime hands back an item whose session's pod could not
// start because the container runtime itself is broken (a
// *sandbox.RuntimeError: its rootless storage poisoned, its user
// namespace refused) with `runtime_broken: <podman's words>` as the
// reason: ReleaseWorkItem, attempts unchanged, the reason its last note
// — the node's condition, never the item's failure (ADR-024, ADR-030).
// It logs one line naming the item and podman's words. As
// ReleaseOnInfrastructure, a refused release is answered by refused (nil:
// FailWorkItem with the same reason, logged), and a cancelled ctx
// releases nothing. It answers the error the session ends with.
func ReleaseOnRuntime(ctx context.Context, gw Gateway, itemID, nodeID, label string, re *sandbox.RuntimeError, refused func(reason string), logf func(string, ...any)) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	reason := RuntimeReason(re)
	wrapped := fmt.Errorf("runtime_broken: %w", re)
	err := gw.ReleaseWorkItem(ctx, itemID, nodeID, reason)
	if err == nil {
		logf("item %s: container runtime broken, released, no attempt charged: %s", label, oneLine(reason))
		return wrapped
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return wrapped
	}
	_ = handInRefused(logf, nil, label, "ReleaseWorkItem", err)
	if refused != nil {
		logf("item %s: container runtime broken and the release was refused: %s", label, oneLine(reason))
		refused(reason)
		return wrapped
	}
	logf("item %s: container runtime broken and the release was refused; failed instead: %s", label, oneLine(reason))
	if err := gw.FailWorkItem(ctx, itemID, reason); !errors.Is(err, context.Canceled) {
		_ = handInRefused(logf, nil, label, "FailWorkItem", err)
	}
	return wrapped
}

// holdRuntime keeps this node off id (an item, or a plan's goal) for
// WorkspaceReleaseHold after a runtime release, the reason the holding
// report's, says until when, and tells hook (nil: no one) — the loop's
// own condition, wired by mesh node start.
func holdRuntime(d *Declines, id, label string, re *sandbox.RuntimeError, hook func(error), logf func(string, ...any)) {
	if d != nil {
		now := time.Now()
		d.HoldFor(id, RuntimeReason(re), now, WorkspaceReleaseHold)
		logf("%s: held off this node until %s after a runtime_broken release", label, now.Add(WorkspaceReleaseHold).UTC().Format(time.RFC3339))
	}
	if hook != nil {
		hook(re)
	}
}

// releaseRuntime is ReleaseOnRuntime for the Runner's item, refused
// failing it, then the hold and the RuntimeBroken hook.
func (r *Runner) releaseRuntime(ctx context.Context, item gateway.WorkItem, label string, re *sandbox.RuntimeError) error {
	sc, _ := r.Scopes.ByOrg(item.OrgID)
	err := ReleaseOnRuntime(ctx, r.GW, item.ID, sc.NodeID, label, re, nil, r.logf)
	if ctx.Err() == nil {
		holdRuntime(r.Declines, item.ID, "item "+label, re, r.RuntimeBroken, r.logf)
	}
	return err
}

// releaseRuntime answers a rebase whose pod could not start on a broken
// runtime: released (ReleaseOnRuntime), a refused release the rebase's
// failure with the same reason, then the hold and the hook.
func (rb *rebaseRun) releaseRuntime(ctx context.Context, re *sandbox.RuntimeError) error {
	sc, _ := rb.r.Scopes.ByOrg(rb.item.OrgID)
	err := ReleaseOnRuntime(ctx, rb.r.GW, rb.item.ID, sc.NodeID, rb.label, re, func(reason string) {
		rb.fail(ctx, errors.New(reason))
	}, rb.r.logf)
	if ctx.Err() == nil {
		holdRuntime(rb.r.Declines, rb.item.ID, "item "+rb.label, re, rb.r.RuntimeBroken, rb.r.logf)
	}
	return err
}

// runtimeNoVerdict answers an evaluation whose pod could not start on a
// broken runtime: an item in evaluating can be neither released nor
// failed by its evaluator, so it is recorded as no verdict
// (EvaluateWorkItemNoVerdict) with the `runtime_broken: …` reason, and
// its notes read an environment failure; a platform without the outcome
// leaves it in evaluating, logged, as before. The hold and the hook
// follow either way.
func (e *Evaluator) runtimeNoVerdict(ctx context.Context, ev evalRun, re *sandbox.RuntimeError, result harness.Result) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	reason := RuntimeReason(re)
	e.logf("item %s: container runtime broken, not evaluated: %s", ev.label, oneLine(reason))
	holdRuntime(e.Declines, ev.item.ID, "item "+ev.label, re, e.RuntimeBroken, e.logf)
	usage := ev.acct.Report(result)
	return e.noVerdict(ctx, ev, reason, result.CostUSD, usage, func() error {
		e.logf("item %s: left in evaluating: %s", ev.label, oneLine(reason))
		return fmt.Errorf("not evaluated, %s", reason)
	})
}

// releaseRuntime answers a plan whose session's pod could not start on
// a broken runtime: the plan claim released with the `runtime_broken:
// …` reason (releasePlan), no try counted, then the hold and the hook.
func (p *Planner) releaseRuntime(ctx context.Context, goal gateway.Goal, scope Scope, re *sandbox.RuntimeError) {
	reason := RuntimeReason(re)
	switch rerr := p.GW.ReleasePlan(ctx, goal.ID, scope.NodeID, reason); {
	case rerr == nil:
		p.logf("goal %s: container runtime broken; plan claim released, no try counted: %s", goal.Slug, oneLine(reason))
	case gateway.IsUnsupported(rerr):
		p.logf("goal %s: container runtime broken; the claim lapses: %s", goal.Slug, oneLine(reason))
	default:
		_ = handInRefused(p.logf, nil, goal.Slug, "ReleasePlan", rerr)
		p.logf("goal %s: container runtime broken; plan release refused (%v): %s", goal.Slug, rerr, oneLine(reason))
	}
	holdRuntime(p.Declines, goal.ID, "goal "+goal.Slug, re, p.RuntimeBroken, p.logf)
}
