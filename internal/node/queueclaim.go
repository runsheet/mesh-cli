package node

import (
	"context"
	"fmt"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The one claim (runsheet ADR 075, one job queue): with a free slot the
// node asks Mesh for the next job in queue order and runs what it is
// handed, instead of reading each kind's candidates and ordering them
// itself. Mesh orders across kinds; the node keeps what only it knows —
// its runtimes, its accounts' headroom, its takes: caps — and hands back
// a job it cannot run. A platform without the one claim is asked again
// after queueRetry; meanwhile the node claims kind by kind as before.

// queueClaimer is the gateway's one claim.
type queueClaimer interface {
	ClaimNext(ctx context.Context, in gateway.ClaimNextInput) (*gateway.ClaimNextAnswer, error)
}

// queueRetry is how long the node claims kind by kind after the platform
// answered it has no one claim.
var queueRetry = time.Hour

// queueFinishTries bounds the evaluations and rebases one tick claims
// past its free slots: work already built, which never waited on a slot
// (evaluateHeld, rebaseRequested).
const queueFinishTries = 8

// The kinds the queue offers today (runsheet #5615).
const (
	queueBuild    = "runsheet/build"
	queueEvaluate = "runsheet/evaluate"
	queueRebase   = "runsheet/rebase"
	queueValidate = "runsheet/validate"
	queuePlan     = "runsheet/plan"
)

// queueOn is whether this tick claims through the one claim: a node with
// one identity (the owner contract) whose platform has not refused it in
// the last queueRetry.
func (l *Loop) queueOn() (queueClaimer, string, bool) {
	o := l.owned()
	if o == nil || o.NodeID == "" {
		return nil, "", false
	}
	qc, ok := l.GW.(queueClaimer)
	if !ok {
		return nil, "", false
	}
	l.mu.Lock()
	off := l.queueOffUntil
	l.mu.Unlock()
	if !off.IsZero() && l.now().Before(off) {
		return nil, "", false
	}
	return qc, o.NodeID, true
}

// claimFromQueue makes up to free one claims, then — past the free slots
// — claims evaluations and rebases as their passes did. It answers false
// when the platform has no one claim: the caller claims kind by kind.
func (l *Loop) claimFromQueue(ctx context.Context, qc queueClaimer, nodeID string, free int) bool {
	for i := 0; i < free; i++ {
		kind := ""
		if !l.anyAccount(ctx) {
			// no account has room for any session: only work that needs
			// none — a validation — can run here this tick
			kind = queueValidate
		}
		claimed, ok := l.claimOne(ctx, qc, nodeID, kind)
		if !ok {
			return false
		}
		if !claimed {
			break
		}
	}
	for _, kind := range []string{queueEvaluate, queueRebase} {
		if !l.takesKind(harness.Kind(kindName(kind))) {
			continue
		}
		for i := 0; i < queueFinishTries; i++ {
			claimed, ok := l.claimOne(ctx, qc, nodeID, kind)
			if !ok {
				return false
			}
			if !claimed {
				break
			}
		}
	}
	return true
}

// claimOne makes one claim and starts what it is handed. claimed is
// false when Mesh had nothing this node could take; ok is false when the
// platform has no one claim.
func (l *Loop) claimOne(ctx context.Context, qc queueClaimer, nodeID, kind string) (claimed, ok bool) {
	a, err := qc.ClaimNext(actorContext(ctx, l.Cfg, gateway.RoleBuilder), gateway.ClaimNextInput{NodeID: nodeID, Harness: l.Harness, Kind: kind})
	if err != nil {
		if gateway.IsUnsupported(err) {
			l.mu.Lock()
			l.queueOffUntil = l.now().Add(queueRetry)
			l.mu.Unlock()
			l.logf("queue: the platform has no one claim; claiming kind by kind for %s", queueRetry)
			return false, false
		}
		l.logf("queue: claim: %v", err)
		return false, true
	}
	if a == nil || a.Job == nil {
		return false, true
	}
	l.startQueued(ctx, nodeID, *a.Job)
	return true, true
}

// kindName is a queue kind's harness kind: "runsheet/build" is build.
func kindName(kind string) string {
	for i := len(kind) - 1; i >= 0; i-- {
		if kind[i] == '/' {
			return kind[i+1:]
		}
	}
	return kind
}

// anyAccount is whether some account has room for a session of any
// priority (the most permissive, urgent), said nowhere: the one claim
// asks only for a validation when none has.
func (l *Loop) anyAccount(ctx context.Context) bool {
	_, ok := l.withAccount(ctx, "", "urgent", func(accounts []AccountConfig, readings map[string]harness.RateLimit) (AccountConfig, bool) {
		return ChooseAccount(l.now(), accounts, readings, l.Thresholds(), "urgent", true)
	})
	return ok
}

// startQueued starts the job the one claim handed this node, or hands it
// back with why when the node cannot run it after all.
func (l *Loop) startQueued(ctx context.Context, nodeID string, job gateway.JobBrief) {
	switch job.Kind {
	case queueBuild:
		l.startQueuedBuild(ctx, nodeID, job.Subject.ID)
	case queueEvaluate:
		l.startQueuedEvaluation(ctx, job.Subject.ID)
	case queueRebase:
		l.startQueuedRebase(ctx, job.Subject.ID)
	case queuePlan:
		l.startQueuedPlan(ctx, nodeID, job.Subject.ID)
	case queueValidate:
		l.startQueuedMain(ctx, nodeID, job.Subject.ID)
	default:
		l.logf("queue: claimed %s %s %s, a kind this node does not run; its claim lapses", job.Kind, job.Subject.Type, job.Subject.ID)
	}
}

// refusedCannotRun is the reason a job is handed back with: the node
// cannot run it, which says nothing of the work.
func refusedCannotRun(why string) string {
	return "refused: cannot run: " + why
}

// queuedItem reads a claimed item and the org it is in.
func (l *Loop) queuedItem(ctx context.Context, id string) (gateway.WorkItem, Scope, bool) {
	it, err := l.GW.WorkItem(ctx, id)
	if err != nil || it == nil {
		l.logf("queue: read claimed item %s: %v", id, err)
		return gateway.WorkItem{}, Scope{}, false
	}
	sc, ok := l.Scopes.ByOrg(it.OrgID)
	if !ok {
		l.logf("queue: claimed item %s is in org %s, which this node does not serve", id, it.OrgID)
		return gateway.WorkItem{}, Scope{}, false
	}
	return *it, sc, true
}

// itemPriority is the item's goal priority, by name.
func (l *Loop) itemPriority(ctx context.Context, sc Scope, item gateway.WorkItem, what string) string {
	return priorityName(l.goalRanks(ctx, sc, what)(item.GoalID))
}

func (l *Loop) startQueuedBuild(ctx context.Context, nodeID, id string) {
	item, sc, ok := l.queuedItem(ctx, id)
	if !ok {
		return
	}
	priority := l.itemPriority(ctx, sc, item, "claimed")
	release := func(why string) {
		l.passOver(item.GoalID, "item", priority, why, time.Time{})
		_ = handInRefused(l.logf, nil, fmt.Sprintf("%s/%d", sc.Name(), item.Seq), "ReleaseWorkItem", l.GW.ReleaseWorkItem(ctx, item.ID, nodeID, refusedCannotRun(why)))
	}
	if runtimes, _ := l.Runtimes(); l.Declines.Holds(item.ID, runtimes, l.now()) {
		release("a tool it needs has no runtime on this node")
		return
	}
	if why, _, held := l.NoCredential.Holds(l.goalProject(ctx, sc, item), string(harness.KindBuild), l.adapter()); held {
		release(why)
		return
	}
	if room, capped := l.kindRoom(harness.KindBuild); capped && room <= 0 {
		release("node.yaml's takes: cap on build is full")
		return
	}
	actx, ok := l.accountFor(ctx, priority, true, "item")
	if !ok {
		release("no account has headroom for a " + priority + " build")
		return
	}
	l.logf("%s: claimed item %d %q from the queue", sc.Name(), item.Seq, item.Title)
	l.start(actx, item, sc.ID)
}

func (l *Loop) startQueuedEvaluation(ctx context.Context, id string) {
	item, sc, ok := l.queuedItem(ctx, id)
	if !ok || l.Evaluate == nil {
		return
	}
	priority := l.itemPriority(ctx, sc, item, "evaluated")
	actx, ok := l.accountToFinish(ctx, priority, "")
	if !ok && l.CodexRoom != nil && l.CodexRoom(priority) {
		actx, ok = withPriority(ctx, priority), true
	}
	if !ok {
		// no command hands an evaluation back: its claim lapses on the
		// platform's timer and another node, or a later tick, takes it
		l.logf("%s: claimed the evaluation of item %d from the queue but no account has headroom for it; its claim lapses", sc.Name(), item.Seq)
		return
	}
	l.mu.Lock()
	if l.evaluating == nil {
		l.evaluating = map[string]bool{}
	}
	if l.evaluating[item.ID] {
		l.mu.Unlock()
		return
	}
	l.evaluating[item.ID] = true
	l.mu.Unlock()
	l.logf("%s: claimed the evaluation of item %d %q from the queue", sc.Name(), item.Seq, item.Title)
	l.wg.Add(1)
	actx, stopped := l.stoppableCtx(actx, "evaluate", item.ID)
	go func(ctx context.Context, item gateway.WorkItem) {
		defer crash.Guard()
		defer l.wg.Done()
		defer stopped()
		defer func() {
			l.mu.Lock()
			delete(l.evaluating, item.ID)
			l.mu.Unlock()
			l.notify()
		}()
		l.Evaluate(ctx, item)
	}(actx, item)
}

func (l *Loop) startQueuedRebase(ctx context.Context, id string) {
	item, sc, ok := l.queuedItem(ctx, id)
	if !ok || l.Rebase == nil {
		return
	}
	priority := l.itemPriority(ctx, sc, item, "rebased")
	actx, ok := l.accountToFinish(ctx, priority, "rebase")
	if !ok {
		l.logf("%s: claimed the rebase of item %d from the queue but no account has headroom for it; its claim lapses", sc.Name(), item.Seq)
		return
	}
	l.mu.Lock()
	if l.rebasing == nil {
		l.rebasing = map[string]bool{}
	}
	if l.rebasing[item.ID] {
		l.mu.Unlock()
		return
	}
	l.rebasing[item.ID] = true
	l.mu.Unlock()
	l.logf("%s: rebasing item %d %q from the queue", sc.Name(), item.Seq, item.Title)
	l.wg.Add(1)
	actx, stopped := l.stoppableCtx(actx, "rebase", item.ID)
	go func(ctx context.Context, item gateway.WorkItem) {
		defer crash.Guard()
		defer l.wg.Done()
		defer stopped()
		defer func() {
			l.mu.Lock()
			delete(l.rebasing, item.ID)
			l.mu.Unlock()
			l.notify()
		}()
		l.Rebase(ctx, item)
	}(actx, item)
}

func (l *Loop) startQueuedPlan(ctx context.Context, nodeID, goalID string) {
	if l.Plan == nil {
		return
	}
	g, sc, ok := l.queuedGoal(ctx, goalID)
	if !ok {
		return
	}
	priority := priorityName(gateway.PriorityRank(g.Priority))
	release := func(why string) {
		l.passOver(g.ID, "plan", priority, why, time.Time{})
		_ = handInRefused(l.logf, nil, g.Slug, "ReleasePlan", l.GW.ReleasePlan(ctx, g.ID, nodeID, refusedCannotRun(why)))
	}
	runtimes, _ := l.Runtimes()
	if why, held := l.Declines.Held(g.ID, runtimes, l.now()); held {
		release(why)
		return
	}
	if why, _, held := l.NoCredential.Holds(g.ProjectID, string(harness.KindPlan), l.adapter()); held {
		release(why)
		return
	}
	actx, ok := l.accountFor(ctx, priority, true, "plan")
	if !ok {
		release("no account has headroom for a " + priority + " plan")
		return
	}
	l.mu.Lock()
	if l.planning == nil {
		l.planning = map[string]bool{}
	}
	if l.planning[g.ID] {
		l.mu.Unlock()
		return
	}
	l.planning[g.ID] = true
	l.mu.Unlock()
	l.logf("%s: planning goal %q from the queue", sc.Name(), g.Slug)
	l.recordClaim(ClaimRecord{NodeID: nodeID, Kind: harness.KindPlan, GoalID: g.ID, ScopeID: sc.ID, Name: g.Slug})
	l.startPlan(actx, g, sc)
}

// queuedGoal finds a claimed goal among the served orgs' goals.
func (l *Loop) queuedGoal(ctx context.Context, goalID string) (gateway.Goal, Scope, bool) {
	for _, sc := range l.Scopes {
		goals, err := l.goals(ctx, sc)
		if err != nil {
			continue
		}
		for _, g := range goals {
			if g.ID == goalID {
				return g, sc, true
			}
		}
	}
	l.logf("queue: claimed goal %s is in no org this node serves; its plan claim lapses", goalID)
	return gateway.Goal{}, Scope{}, false
}

func (l *Loop) startQueuedMain(ctx context.Context, nodeID, itemID string) {
	gw, ok := l.mainValidation(ctx)
	if !ok || l.ValidateMain == nil {
		return
	}
	q, err := gw.MainValidations(ctx, nodeID)
	if err != nil {
		l.logf("queue: read main validations: %v", err)
		return
	}
	for _, r := range q.Requests {
		if r.WorkItemID != itemID {
			continue
		}
		j := requestJob(l, gw, r, nodeID)
		// the one claim already claimed it: the job starts with no claim
		// of its own
		j.claim = func(context.Context) error { return nil }
		j.claimed = "claimed from the queue"
		l.startMainJob(ctx, j, false)
		return
	}
	// the queue read lags the claim by a step: the claim lapses on the
	// platform's timer and the validation is offered again
	l.logf("queue: claimed main validation of item %s is not in this node's queue yet; its claim lapses", itemID)
}
