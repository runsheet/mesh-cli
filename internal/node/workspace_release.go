package node

import (
	"context"
	"errors"
	"fmt"
	"time"
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
