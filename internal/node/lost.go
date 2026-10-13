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

// claimWatch is a build's lost-claim and starve checks: the session's
// ctx and the reason it was given up, if it was — its claim lost (why,
// with the host's overload reason when it was overloaded at the time),
// or starved on an overloaded host (starvedWhy).
type claimWatch struct {
	cancel     context.CancelCauseFunc
	mu         sync.Mutex
	why        string
	overload   string
	starvedWhy string
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
	w.loseOverloaded(why, "")
}

// loseOverloaded is lose with the host's overload reason at the time,
// "" when the host was not overloaded: the platform stalled the item,
// but the host starved it (ADR-029).
func (w *claimWatch) loseOverloaded(why, overload string) {
	if w == nil || why == "" {
		return
	}
	w.mu.Lock()
	if w.why == "" && w.starvedWhy == "" {
		w.why, w.overload = why, overload
	}
	w.mu.Unlock()
	w.cancel(ErrClaimLost)
}

// starve gives the session up as starved on an overloaded host: its ctx
// is cancelled with ErrStarved, which stops and kills its unit, and the
// item is released with the overload reason (afterStarved). A claim
// already lost stays lost.
func (w *claimWatch) starve(overload string) {
	if w == nil || overload == "" {
		return
	}
	w.mu.Lock()
	if w.why == "" && w.starvedWhy == "" {
		w.starvedWhy = overload
	}
	w.mu.Unlock()
	w.cancel(ErrStarved)
}

// starved is the overload reason the session was starved for, "" when
// it was not.
func (w *claimWatch) starved() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.starvedWhy
}

// overloadAtLoss is the host's overload reason when the claim was lost,
// "" when the host was not overloaded then.
func (w *claimWatch) overloadAtLoss() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overload
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
			b.watch.loseOverloaded(why, b.overloadNow())
			return false
		}
	}
	if why := lostRefusal(b.report(ctx, "")); why != "" {
		b.watch.loseOverloaded(why, b.overloadNow())
		return false
	}
	return true
}

// overloadNow is the host's overload reason now, "" when it is not
// overloaded: a claim the platform gave up while the host starved the
// session says so, so the item is not blamed (ADR-029).
func (b *buildRun) overloadNow() string {
	if over, why := b.r.overloaded(); over {
		return why
	}
	return ""
}

// afterLost ends a build whose claim the node lost: what its unit's kill
// found is logged, its session directory goes, and nothing is sent for
// the item — no fail, no finish, no release: it is not the node's. It
// is no error of the node's either: the platform gave the item up.
//
// A claim lost while the host was overloaded names the overload too —
// `claim lost (item is ready; host overloaded: load/core …)` — so the
// operator sees the item was not to blame (ADR-029).
func (r *Runner) afterLost(b *buildRun, why string, result harness.Result) error {
	if over := b.watch.overloadAtLoss(); over != "" {
		why += "; " + over
	}
	r.logf("item %s: claim lost (%s); session stopped", b.label, why)
	logKilled(r.logf, "item "+b.label, result.KilledPerSession(), result.KillError)
	_ = harness.RemoveSession(b.dir)
	return nil
}
