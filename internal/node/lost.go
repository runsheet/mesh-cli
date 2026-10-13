package node

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// ErrClaimLost is the cause a build's session ctx is cancelled with when
// the node no longer holds its item — the platform stalled it, its lease
// ended, it was released or cancelled. It Is harness.ErrStopSession, so
// an Outlive session is stopped and its unit killed rather than detached
// for a next node that will never adopt it (goal
// a-stalled-session-is-stopped-not-orphaned).
var ErrClaimLost = fmt.Errorf("claim lost: %w", harness.ErrStopSession)

// lostRefusal is why a refused progress heartbeat says the node no
// longer holds the item, or "" when the error says nothing of the kind:
// a transport error, a timeout, an older platform, or a refusal for
// another reason is never a lost claim. The platform refuses a heartbeat
// on an item not claimed or running as `item is <status>, not running`,
// one another node holds as `not yours` or `held by …`, and a cancelled
// one by its status.
func lostRefusal(err error) string {
	if err == nil || !gateway.IsRefusal(err) || gateway.IsUnsupported(err) {
		return ""
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"not running", "not held", "not claimed", "not yours", "held by", "cancelled", "canceled", "no such item"} {
		if strings.Contains(msg, s) {
			return "progress refused: " + err.Error()
		}
	}
	return ""
}

// lostByRead is why the item as the platform reads it is no longer this
// node's build, or "": terminal or cancelled, back in ready or pending,
// or claimed or running on another node. nodeID "" checks the status
// alone.
func lostByRead(item *gateway.WorkItem, nodeID string) string {
	if item == nil {
		return "item not on the platform"
	}
	switch item.Status {
	case "claimed", "running":
	default:
		return "item is " + item.Status
	}
	if nodeID != "" && item.NodeID != "" && item.NodeID != nodeID {
		return "item is " + item.Status + " on node " + item.NodeID
	}
	return ""
}

// claimWatch is a build's lost-claim check: the session's ctx and the
// reason it was given up, if it was.
type claimWatch struct {
	cancel context.CancelCauseFunc
	mu     sync.Mutex
	why    string
}

// newClaimWatch derives the session's ctx from ctx, cancelled with
// ErrClaimLost when lose is called.
func newClaimWatch(ctx context.Context) (context.Context, *claimWatch) {
	sctx, cancel := context.WithCancelCause(ctx)
	return sctx, &claimWatch{cancel: cancel}
}

// lose gives the session up for why: its ctx is cancelled with
// ErrClaimLost, which stops and kills its unit. The first reason is
// kept.
func (w *claimWatch) lose(why string) {
	if w == nil || why == "" {
		return
	}
	w.mu.Lock()
	if w.why == "" {
		w.why = why
	}
	w.mu.Unlock()
	w.cancel(ErrClaimLost)
}

// lost is the reason the session was given up, "" while it is held.
func (w *claimWatch) lost() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.why
}

// release frees the derived ctx once the session is over.
func (w *claimWatch) release() {
	if w != nil {
		w.cancel(nil)
	}
}

// checkClaim is one tick of a build's lost-claim check, at most once per
// ProgressEvery: the item read back (a read that fails says nothing), and
// the heartbeat sent only while it is still this node's — a heartbeat
// refused as not held loses it too. It reports whether the claim is
// still held.
func (b *buildRun) checkClaim(ctx context.Context) bool {
	if it, err := b.r.GW.WorkItem(ctx, b.item.ID); err == nil {
		if why := lostByRead(it, b.nodeID); why != "" {
			b.watch.lose(why)
			return false
		}
	}
	if why := lostRefusal(b.report(ctx, "")); why != "" {
		b.watch.lose(why)
		return false
	}
	return true
}

// afterLost ends a build whose claim the node lost: what its unit's kill
// found is logged, its session directory goes, and nothing is sent for
// the item — no fail, no finish, no release: it is not the node's. It
// is no error of the node's either: the platform gave the item up.
func (r *Runner) afterLost(b *buildRun, why string, result harness.Result) error {
	r.logf("item %s: claim lost (%s); session stopped", b.label, why)
	logKilled(r.logf, "item "+b.label, result.KilledPerSession(), result.KillError)
	_ = harness.RemoveSession(b.dir)
	return nil
}
