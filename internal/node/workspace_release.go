package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/tripline"
)

// WorkspaceReleaseHold is how long a node keeps off an item it released
// for a workspace that could not be prepared, so an item whose
// workspace always fails here does not spin between claim and release
// (ADR-024).
const WorkspaceReleaseHold = 10 * time.Minute

// ReleaseOnWorkspace hands an item whose workspace could not be
// prepared — a fetch, a checkout, the context written into it — back to
// the platform (ReleaseWorkItem, attempts unchanged) with `workspace:
// <error>` as the reason instead of failing it: a workspace failure
// before the harness starts is the node's condition, not the item's
// (ADR-024). As ReleaseOnCredential, a platform that refuses the release
// would leave the item held by a node that has stopped working on it,
// so then the item is failed with the same reason, logged. A cancelled
// ctx (the node's own shutdown) releases and fails nothing. It answers
// the error the item's session ends with.
func ReleaseOnWorkspace(ctx context.Context, gw Gateway, itemID, nodeID, label string, cause error, logf func(string, ...any)) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cause = scrubbed(cause, nil)
	reason := truncate("workspace: "+cause.Error(), 2000)
	wrapped := fmt.Errorf("workspace: %w", cause)
	err := gw.ReleaseWorkItem(ctx, itemID, nodeID, reason)
	if err == nil {
		logf("item %s: workspace failed, released for a retry: %s", label, oneLine(cause.Error()))
		return wrapped
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return wrapped
	}
	logf("item %s: workspace failed and the release was refused (%v); failed instead: %s", label, err, oneLine(reason))
	if err := gw.FailWorkItem(ctx, itemID, reason); err != nil && !errors.Is(err, context.Canceled) {
		logf("item %s: fail: %v", label, err)
	}
	return wrapped
}

// releaseWorkspace is ReleaseOnWorkspace for the Runner's item, and on a
// release the platform took, the hold that keeps this node off it for
// WorkspaceReleaseHold.
func (r *Runner) releaseWorkspace(ctx context.Context, item, label string, nodeID string, cause error) error {
	err := ReleaseOnWorkspace(ctx, r.GW, item, nodeID, label, cause, r.logf)
	if ctx.Err() == nil {
		reportFailure(r.Tripline, tripline.Infrastructure, err, gateway.WorkItem{ID: item}, label, "", gateway.RoleBuilder)
	}
	if ctx.Err() != nil {
		return err
	}
	now := time.Now()
	r.Declines.HoldFor(item, err.Error(), now, WorkspaceReleaseHold)
	if r.Declines != nil {
		r.logf("item %s: held off this node until %s after a workspace release", label, now.Add(WorkspaceReleaseHold).UTC().Format(time.RFC3339))
	}
	return err
}

// InfrastructurePrefix opens the reason an infrastructure release hands
// the platform, and the holding report's entry for the item it holds.
const InfrastructurePrefix = "infrastructure: "

// ReleaseOnInfrastructure hands back an item whose session could not
// start for the node's own fault before any workspace was prepared —
// the item's token could not be written into the repo cache
// (NewItemCred's *WorkspaceError: a stale .git/config.lock) — with
// `infrastructure: <error>` as the reason: ReleaseWorkItem, attempts
// unchanged, the reason its last note, so the platform does not re-plan
// around a broken node. It logs one line naming the repo cache. As
// ReleaseOnWorkspace, a refused release is answered by refused (nil:
// FailWorkItem with the same reason, logged), and a cancelled ctx
// releases nothing. It answers the error the session ends with.
func ReleaseOnInfrastructure(ctx context.Context, gw Gateway, itemID, nodeID, label string, we *WorkspaceError, refused func(reason string), logf func(string, ...any)) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cause := scrubbed(we, nil)
	reason := truncate(InfrastructurePrefix+cause.Error(), 2000)
	wrapped := fmt.Errorf("infrastructure: %w", we)
	err := gw.ReleaseWorkItem(ctx, itemID, nodeID, reason)
	if err == nil {
		logf("item %s: repo cache %s failed, released as an infrastructure hold, no attempt charged: %s", label, we.Path, oneLine(cause.Error()))
		return wrapped
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return wrapped
	}
	if refused != nil {
		logf("item %s: repo cache %s failed and the release was refused (%v): %s", label, we.Path, err, oneLine(reason))
		refused(reason)
		return wrapped
	}
	logf("item %s: repo cache %s failed and the release was refused (%v); failed instead: %s", label, we.Path, err, oneLine(reason))
	if err := gw.FailWorkItem(ctx, itemID, reason); err != nil && !errors.Is(err, context.Canceled) {
		logf("item %s: fail: %v", label, err)
	}
	return wrapped
}

// holdInfrastructure keeps this node off itemID for WorkspaceReleaseHold
// after an infrastructure release, the reason the holding report's
// (Loop.passOver), and says until when.
func holdInfrastructure(d *Declines, itemID, label string, cause error, logf func(string, ...any)) {
	if d == nil {
		return
	}
	now := time.Now()
	d.HoldFor(itemID, truncate(InfrastructurePrefix+scrubbed(cause, nil).Error(), 2000), now, WorkspaceReleaseHold)
	logf("item %s: held off this node until %s after an infrastructure release", label, now.Add(WorkspaceReleaseHold).UTC().Format(time.RFC3339))
}

// releaseInfrastructure is ReleaseOnInfrastructure for the Runner's
// item, refused failing it, and the hold that follows.
func (r *Runner) releaseInfrastructure(ctx context.Context, item gateway.WorkItem, label string, we *WorkspaceError) error {
	sc, _ := r.Scopes.ByOrg(item.OrgID)
	err := ReleaseOnInfrastructure(ctx, r.GW, item.ID, sc.NodeID, label, we, nil, r.logf)
	if ctx.Err() == nil {
		reportFailure(r.Tripline, tripline.Infrastructure, err, item, label, we.Repo, gateway.RoleBuilder)
		holdInfrastructure(r.Declines, item.ID, label, we, r.logf)
	}
	return err
}
