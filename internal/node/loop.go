package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Loop is trd §3: every Interval, heartbeat; list the orgs' ready items;
// claim up to capacity − running, most urgent goal first; hand each claim to Dispatch. The claim
// is the lock — it is taken here, before anything runs, in both modes.
type Loop struct {
	GW  Gateway
	Cfg *Config
	// Scopes are the orgs served: each is heartbeaten, listed and
	// claimed from in turn, under the one capacity.
	Scopes   Scopes
	Version  string
	Harness  string
	Interval time.Duration
	Log      *log.Logger
	// Dispatch runs a claimed item and returns when it is over; Loop
	// counts it as running meanwhile.
	Dispatch func(ctx context.Context, item gateway.WorkItem)
	// Release is called after Dispatch returns (optional).
	Release func(item gateway.WorkItem)
	// Evaluate reviews an item this node finished (status evaluating,
	// node_id ours) and returns when the verdict is recorded — or not,
	// in which case the next tick offers it again. Optional; nil leaves
	// evaluation to someone else. It does not count against capacity.
	Evaluate func(ctx context.Context, item gateway.WorkItem)
	// Plan compiles a goal in planning (in one of the served orgs) into
	// its graph and returns when the plan is handed in — or not, in
	// which case the next tick offers it again. Optional.
	Plan func(ctx context.Context, goal gateway.Goal, scope Scope)
	// Research reads one direction of a project and proposes the goals
	// it asks for (runsheet trd §6c). The claim is taken here, before it
	// runs; the platform refuses it outside the direction's hours,
	// inside its gap, or when another node holds it, so a refusal is the
	// ordinary answer. Optional; it does not count against capacity — it
	// competes for the machine, not for the work.
	Research func(ctx context.Context, direction gateway.Direction, project gateway.Project, scope Scope)
	// Rebase runs a session of kind rebase on an item whose rebase this
	// node has claimed (runsheet ADR 008) and returns when the rebase is
	// finished or failed. The claim is taken here, before it runs; another
	// node's claim is a refusal, the ordinary answer. Optional; like an
	// evaluation it does not count against capacity.
	Rebase func(ctx context.Context, item gateway.WorkItem)
	// AfterTick runs at the end of every tick (optional).
	AfterTick func(ctx context.Context)
	// Reload is asked at the top of every tick for the capacity, the
	// limits thresholds and the orgs served as they stand now (node.yaml
	// and MyOrgs), so none needs a restart (optional; Reloader builds
	// it). A capacity of 0, nil limits or nil scopes keep what the loop
	// has; an error is logged, and what came back with it is still taken.
	Reload func(ctx context.Context) (Reloaded, error)
	// Limits holds the latest Claude plan standing of each account, fed
	// by every session (ObserveLimits); the heartbeat carries it and the
	// loop paces by it. Nil until the first tick makes one.
	Limits *Limits
	// Now is the clock pacing and scaling read (time.Now when nil).
	Now func() time.Time
	// Windows are the plan's windows the scaler paces a ranged capacity
	// by (adr-004); nil reads them from Limits, the highest utilization
	// across accounts per window.
	Windows WindowSource
	// Machine is the host the scaler caps a ranged capacity by; nil
	// imposes no cap (HostMachine is this one).
	Machine MachineProbe

	mu          sync.Mutex
	running     map[string]bool
	evaluating  map[string]bool
	planning    map[string]bool
	researching map[string]bool
	rebasing    map[string]bool
	wg          sync.WaitGroup
	// draining is set by Drain: nothing new is started, and Run returns
	// once every session of any kind has ended.
	draining bool
	// wake tells Run that a session ended or a drain began.
	wake chan struct{}
	// capacity is the one in force: a fixed node's number, or where a
	// ranged node's scaler has stepped it to (0 until the first tick).
	capacity int
	// capRange is node.yaml's capacity as last read: Cfg.Capacity until
	// Reload says otherwise (zero).
	capRange Capacity
	// scaling is the scaler's state while capRange is a range.
	scaling scaleState
	// limits are the thresholds in force: Cfg.Limits until Reload says
	// otherwise (limitsSet).
	limits    LimitsConfig
	limitsSet bool
	// paced is the pacing mode last logged, so a crossing is logged once.
	paced paceMode
	// waiting counts, by kind, the work this tick left unclaimed because
	// no account had headroom for it: logged once at the tick's end.
	waiting map[string]int
	// tickGoals holds each org's goals as read this tick, so the claim,
	// the planner and the director share one GoalIndex read per org.
	tickGoals map[string]goalsRead
}

type goalsRead struct {
	goals []gateway.Goal
	err   error
}

// goals reads the org's goals once per tick.
func (l *Loop) goals(ctx context.Context, sc Scope) ([]gateway.Goal, error) {
	if r, ok := l.tickGoals[sc.ID]; ok {
		return r.goals, r.err
	}
	goals, err := l.GW.Goals(ctx, sc.ID)
	if l.tickGoals != nil {
		l.tickGoals[sc.ID] = goalsRead{goals, err}
	}
	return goals, err
}

// readyItem is a ready item with the scope it was listed in and its
// goal's priority rank.
type readyItem struct {
	item  gateway.WorkItem
	scope Scope
	rank  int
}

// claimOrder sorts ready items from every org into the order they are
// claimed: by the goal's priority (urgent, high, normal, low), then
// fresh items before retries (a retry carries an evaluation), then seq.
// The sort is stable, so ties keep the order the orgs were listed in.
func claimOrder(items []readyItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if ra, rb := a.item.Evaluation != "", b.item.Evaluation != ""; ra != rb {
			return rb
		}
		return a.item.Seq < b.item.Seq
	})
}

// goalRanks joins the org's goals to their priority rank through the
// tick's one GoalIndex read. A goal it does not know — or every goal,
// when the read fails — ranks as normal; what says what the org's work
// is then done as normal ("claimed", "evaluated").
func (l *Loop) goalRanks(ctx context.Context, sc Scope, what string) func(goalID string) int {
	priority := map[string]string{}
	goals, err := l.goals(ctx, sc)
	if err != nil {
		l.logf("list goals of %s (its items %s as normal): %v", sc.Login, what, err)
	}
	for _, g := range goals {
		priority[g.ID] = g.Priority
	}
	return func(goalID string) int { return gateway.PriorityRank(priority[goalID]) }
}

// evaluateOrder sorts the items held for evaluation from every org by
// their goal's priority, then seq.
func evaluateOrder(items []readyItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.item.Seq < b.item.Seq
	})
}

// rebaseOrder sorts the items whose rebase is requested from every org
// by their goal's priority, then seq: the order they are claimed in,
// as the rest of the node's work is.
func rebaseOrder(items []readyItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.item.Seq < b.item.Seq
	})
}

// planningGoal is a goal in planning with the scope it was listed in.
type planningGoal struct {
	goal  gateway.Goal
	scope Scope
}

// planOrder sorts the goals in planning from every org by priority. The
// Goal carries no age, so the sort is stable: ties keep the order the
// orgs and their goals were listed in.
func planOrder(goals []planningGoal) {
	sort.SliceStable(goals, func(i, j int) bool {
		return gateway.PriorityRank(goals[i].goal.Priority) < gateway.PriorityRank(goals[j].goal.Priority)
	})
}

// claimReady lists every served org's ready items, joins each to its
// goal's priority (one GoalIndex read per org, none for an org with
// nothing ready) and claims them in claimOrder until free runs out, each
// on the account chosen for its priority; items of a goal ranked past
// maxRank are left. An item no account has headroom for is not
// claimed: it stays ready for a later tick.
func (l *Loop) claimReady(ctx context.Context, free, maxRank int) {
	var ready []readyItem
	for _, sc := range l.Scopes {
		items, err := l.GW.ReadyWorkItems(ctx, sc.ID)
		if err != nil {
			l.logf("list ready items of %s: %v", sc.Login, err)
			continue
		}
		if len(items) == 0 {
			continue
		}
		rank := l.goalRanks(ctx, sc, "claimed")
		for _, it := range items {
			if r := rank(it.GoalID); r <= maxRank {
				ready = append(ready, readyItem{item: it, scope: sc, rank: r})
			}
		}
	}
	claimOrder(ready)
	for _, r := range ready {
		if free <= 0 {
			break
		}
		item, sc := r.item, r.scope
		l.mu.Lock()
		already := l.running[item.ID]
		l.mu.Unlock()
		if already {
			continue
		}
		actx, ok := l.accountFor(ctx, priorityName(r.rank), true, "item")
		if !ok {
			continue
		}
		if err := l.GW.ClaimWorkItem(ctx, item.ID, sc.NodeID, l.Harness); err != nil {
			if gateway.IsRefusal(err) {
				l.logf("%s item %d %q: not ours (%v)", sc.Login, item.Seq, item.Title, err)
			} else {
				l.logf("%s claim item %d: %v", sc.Login, item.Seq, err)
			}
			continue
		}
		free--
		l.logf("%s: claimed item %d %q", sc.Login, item.Seq, item.Title)
		l.start(actx, item)
	}
}

func (l *Loop) logf(format string, args ...any) {
	if l.Log != nil {
		l.Log.Printf(format, args...)
	}
}

// Running is the number of items this node is building: what counts
// against capacity. Evaluations, plans, research and rebases do not.
func (l *Loop) Running() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.running)
}

// RunningIDs lists the items in flight.
func (l *Loop) RunningIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, 0, len(l.running))
	for id := range l.running {
		ids = append(ids, id)
	}
	return ids
}

// Run ticks until ctx is done, then waits for the items in flight (their
// harnesses are killed by the same ctx) and returns ctx's error. Once
// Drain has been called it returns nil instead, as soon as no session of
// any kind is left — items, evaluations, plans, research and rebases alike, since
// only the items count against capacity and a node is rarely without
// the others.
func (l *Loop) Run(ctx context.Context) error {
	l.mu.Lock()
	if l.running == nil {
		l.running = map[string]bool{}
	}
	l.mu.Unlock()
	interval := l.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	wake := l.wakeup()
	l.Tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if l.drained() {
			l.wg.Wait() // the last session's Release
			l.logf("drained: every session has ended")
			return nil
		}
		select {
		case <-ctx.Done():
			l.wg.Wait()
			return ctx.Err()
		case <-t.C:
			l.Tick(ctx)
		case <-wake:
		}
	}
}

// Drain stops the node taking anything new — items, evaluations, plans,
// research, rebases — and lets Run return once what is running has ended.
// Calling it again does nothing.
func (l *Loop) Drain() {
	l.mu.Lock()
	if l.draining {
		l.mu.Unlock()
		return
	}
	l.draining = true
	n := l.sessions()
	l.mu.Unlock()
	l.logf("draining: nothing new is claimed; %d session(s) to finish", n)
	l.notify()
}

// Draining reports whether Drain has been called.
func (l *Loop) Draining() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.draining
}

// drained is draining with no session of any kind left.
func (l *Loop) drained() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.draining && l.sessions() == 0
}

// sessions counts every session in flight, of any kind; l.mu is held.
func (l *Loop) sessions() int {
	return len(l.running) + len(l.evaluating) + len(l.planning) + len(l.researching) + len(l.rebasing)
}

func (l *Loop) wakeup() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wake == nil {
		l.wake = make(chan struct{}, 1)
	}
	return l.wake
}

// notify wakes Run without blocking: a session ended, or a drain began.
func (l *Loop) notify() {
	select {
	case l.wakeup() <- struct{}{}:
	default:
	}
}

// Tick is one round: heartbeat, list, claim, dispatch. A draining node
// only heartbeats — with draining set — (and runs AfterTick): it claims no
// item, plan, research or rebase and starts no evaluation — an item it holds for evaluation
// waits in evaluating for the node that comes up after it. Every
// heartbeat carries the plan limits last read, and every start runs on
// the account ChooseAccount picks by them: an account paced near its
// five-hour window's limit takes only urgent and high work, one stopped
// near either window's end takes nothing, and work no account has room
// for is not claimed — like a drain for new starts, but the process runs
// on. The node's own mode (nodePace) is logged at each crossing and, when
// paced, halves the capacity.
func (l *Loop) Tick(ctx context.Context) {
	l.mu.Lock()
	if l.running == nil {
		l.running = map[string]bool{}
	}
	l.mu.Unlock()
	l.tickGoals = map[string]goalsRead{}
	l.reload(ctx)
	l.scale()
	extras := gateway.HeartbeatExtras{Draining: l.Draining(), Limits: l.limitsStore().Heartbeat()}
	for _, sc := range l.Scopes {
		if err := l.GW.HeartbeatNode(ctx, sc.NodeID, l.Version, l.Running(), extras); err != nil {
			l.logf("heartbeat %s: %v", sc.Login, err)
		}
	}
	p := l.pacing()
	if !l.Draining() {
		// every start below first picks its account (accountFor): one
		// with no account to run on is left for a later tick
		l.waiting = map[string]int{}
		l.evaluateHeld(ctx)
		l.rebaseRequested(ctx)
		l.planRequested(ctx)
		l.researchDue(ctx)
		free := l.Capacity() - l.Running()
		if p.mode == pacePaced {
			free = pacedFree(free, l.Capacity(), l.Running())
		}
		// a ranged node at its min builds only urgent and high goals
		claimRank := gateway.PriorityRank("low")
		if l.atMin() {
			claimRank = min(claimRank, gateway.PriorityRank("high"))
		}
		if free > 0 {
			l.claimReady(ctx, free, claimRank)
		}
		l.logWaiting()
	}
	if l.AfterTick != nil {
		l.AfterTick(ctx)
	}
}

// drainPoll is how often DrainAndWait looks for the last session to end.
var drainPoll = 50 * time.Millisecond

// errResumed is DrainAndWait's answer when Resume ends the drain first.
var errResumed = errors.New("drain resumed")

// DrainAndWait drains the loop and returns once no session of any kind
// is left, with ctx.Err() when ctx is done first, or with an error when
// Resume ends the drain first. It is the drain a self-update waits on
// before it swaps the binary; Run still returns on its own meanwhile.
func (l *Loop) DrainAndWait(ctx context.Context) error {
	l.Drain()
	t := time.NewTicker(drainPoll)
	defer t.Stop()
	for {
		if l.drained() {
			return nil
		}
		if !l.Draining() {
			return errResumed
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Resume undoes a drain that has not yet ended Run: the next tick claims
// again, and the heartbeat stops reporting draining.
func (l *Loop) Resume() {
	l.mu.Lock()
	was := l.draining
	l.draining = false
	l.mu.Unlock()
	if was {
		l.logf("resumed: claiming again")
	}
}

// Capacity is the number of items this node runs at once: the capacity
// in force, as last read or scaled. Before the first tick a ranged node
// has its min; once scaling, a busy machine may hold it below (even 0).
func (l *Loop) Capacity() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.capacity > 0 || l.scaling.scaler != nil {
		return l.capacity
	}
	if r := l.rangeLocked(); r.IsRange() {
		return r.Min
	}
	return l.rangeLocked().Max
}

// Thresholds are the limits thresholds in force, as last read.
func (l *Loop) Thresholds() LimitsConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limitsSet {
		return l.limits.Thresholds()
	}
	return l.Cfg.Limits.Thresholds()
}

// limitsStore is Limits, made on first use.
func (l *Loop) limitsStore() *Limits {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Limits == nil {
		l.Limits = &Limits{}
	}
	return l.Limits
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// pacing decides this tick's mode from the readings and logs a
// crossing into another mode — once, not every tick.
func (l *Loop) pacing() pacing {
	accounts, readings := l.accounts()
	p := nodePace(l.now(), accounts, readings, l.Thresholds())
	l.mu.Lock()
	was := l.paced
	l.paced = p.mode
	n := l.sessions()
	l.mu.Unlock()
	if p.mode == was {
		return p
	}
	switch p.mode {
	case pacePaced:
		l.logf("limits: paced until %s: %s — only urgent and high goals are claimed and planned, at most %d item(s) at once, and no research", p.resumeAt.UTC().Format(time.RFC3339), p.why, max(1, l.Capacity()/2))
	case paceStopped:
		also := ""
		for _, a := range accounts {
			if a.Kind == AccountAPI {
				also = " on a subscription (an api account takes urgent and high work, and work that waits)"
				break
			}
		}
		l.logf("limits: stopped until %s: %s — nothing new is started%s; %d session(s) to finish", p.resumeAt.UTC().Format(time.RFC3339), p.why, also, n)
	default:
		l.logf("limits: back to normal (was %s)", was)
	}
	return p
}

// accounts are the accounts the node chooses among and their latest
// readings by label. With none configured it is the one implicit
// account, the operator's own login, judged by its strictest reading.
func (l *Loop) accounts() ([]AccountConfig, map[string]harness.RateLimit) {
	readings := l.limitsStore().Readings()
	if len(l.Cfg.Accounts) > 0 {
		return l.Cfg.Accounts, readings
	}
	own := []AccountConfig{{Label: DefaultAccount, Kind: AccountSubscription}}
	if r, ok := strictest(l.now(), readings, l.Thresholds()); ok {
		return own, map[string]harness.RateLimit{DefaultAccount: r}
	}
	return own, nil
}

// accountFor picks the account a session of that priority runs on
// (ChooseAccount) and returns ctx carrying it for OpenAccount — the
// operator's own login, when none is configured, carries nothing. ok is
// false when no account has headroom: the session is not started, and
// a what that is not empty is counted for the tick's one line
// (logWaiting).
func (l *Loop) accountFor(ctx context.Context, priority string, workWaits bool, what string) (context.Context, bool) {
	return l.withAccount(ctx, what, func(accounts []AccountConfig, readings map[string]harness.RateLimit) (AccountConfig, bool) {
		return ChooseAccount(l.now(), accounts, readings, l.Thresholds(), priority, workWaits)
	})
}

// accountToFinish picks the account for an evaluation or a rebase —
// work already built, which pacing never held back: by the spending
// order for its priority first, and failing that a paced subscription
// as if it were pressing. A stopped account still takes nothing, and an
// api account is still only what its priority or exhausted
// subscriptions allow.
func (l *Loop) accountToFinish(ctx context.Context, priority, what string) (context.Context, bool) {
	return l.withAccount(ctx, what, func(accounts []AccountConfig, readings map[string]harness.RateLimit) (AccountConfig, bool) {
		if a, ok := ChooseAccount(l.now(), accounts, readings, l.Thresholds(), priority, true); ok {
			return a, true
		}
		var subs []AccountConfig
		for _, a := range accounts {
			if a.Kind != AccountAPI {
				subs = append(subs, a)
			}
		}
		return ChooseAccount(l.now(), subs, readings, l.Thresholds(), "high", true)
	})
}

func (l *Loop) withAccount(ctx context.Context, what string, choose func([]AccountConfig, map[string]harness.RateLimit) (AccountConfig, bool)) (context.Context, bool) {
	accounts, readings := l.accounts()
	a, ok := choose(accounts, readings)
	if !ok {
		if what != "" && l.waiting != nil {
			l.waiting[what]++
		}
		return ctx, false
	}
	if len(l.Cfg.Accounts) == 0 {
		return ctx, true
	}
	return WithAccount(ctx, a), true
}

// logWaiting says once per tick what waits for an account with
// headroom, not once per item.
func (l *Loop) logWaiting() {
	if len(l.waiting) == 0 {
		return
	}
	kinds := make([]string, 0, len(l.waiting))
	for k := range l.waiting {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s(s)", l.waiting[k], k))
	}
	l.logf("accounts: no account has headroom for %s; left unclaimed until one has", strings.Join(parts, ", "))
}

// priorityName is the priority a goal rank stands for.
func priorityName(rank int) string {
	return [...]string{"urgent", "high", "normal", "low"}[min(max(rank, 0), 3)]
}

// CurrentScopes are the orgs served, as last read — for the sessions
// that need an item's org login after the loop has started.
func (l *Loop) CurrentScopes() Scopes {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Scopes
}

// reload takes the capacity, the limits and the orgs from Reload, logging a change.
// Tick is the only writer of Scopes, so its own reads need no lock.
func (l *Loop) reload(ctx context.Context) {
	if l.Reload == nil {
		return
	}
	got, err := l.Reload(ctx)
	scopes := got.Scopes
	if err != nil {
		l.logf("reload config: %v", err)
	}
	if got.Capacity.Max > 0 {
		l.setRange(got.Capacity)
	}
	if got.Limits != nil {
		was, now := l.Thresholds(), got.Limits.Thresholds()
		if now != was {
			l.logf("limits: pace five-hour %s → %s, stop %s → %s", pct(was.PaceFiveHour), pct(now.PaceFiveHour), pct(was.Stop), pct(now.Stop))
		}
		l.mu.Lock()
		l.limits, l.limitsSet = *got.Limits, true
		l.mu.Unlock()
	}
	if scopes != nil && !sameOrgs(scopes, l.Scopes) {
		l.logf("orgs %s → %s", strings.Join(l.Scopes.Logins(), ", "), strings.Join(scopes.Logins(), ", "))
		l.mu.Lock()
		l.Scopes = scopes
		l.mu.Unlock()
	}
}

func sameOrgs(a, b Scopes) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// evaluateHeld starts an evaluation of every item this node finished
// that no evaluation here is running on, most urgent goal first across
// the orgs (evaluateOrder; the goals are read only for an org holding
// one). The verdict moves the item on;
// an evaluation that ends without one leaves it here for the next tick.
func (l *Loop) evaluateHeld(ctx context.Context) {
	if l.Evaluate == nil {
		return
	}
	var held []readyItem
	for _, sc := range l.Scopes {
		rows, err := l.GW.OrgWorkItems(ctx, sc.ID, "evaluating")
		if err != nil {
			l.logf("list evaluating items of %s: %v", sc.Login, err)
			continue
		}
		var rank func(string) int
		for _, it := range rows {
			if it.NodeID != sc.NodeID {
				continue
			}
			if rank == nil {
				rank = l.goalRanks(ctx, sc, "evaluated")
			}
			held = append(held, readyItem{item: it, scope: sc, rank: rank(it.GoalID)})
		}
	}
	evaluateOrder(held)
	for _, h := range held {
		item := h.item
		l.mu.Lock()
		if l.evaluating == nil {
			l.evaluating = map[string]bool{}
		}
		if l.evaluating[item.ID] {
			l.mu.Unlock()
			continue
		}
		l.mu.Unlock()
		actx, ok := l.accountToFinish(ctx, priorityName(h.rank), "evaluation")
		if !ok {
			continue
		}
		l.mu.Lock()
		l.evaluating[item.ID] = true
		l.mu.Unlock()
		l.logf("evaluating item %d %q", item.Seq, item.Title)
		l.wg.Add(1)
		go func(ctx context.Context, item gateway.WorkItem) {
			defer l.wg.Done()
			defer func() {
				l.mu.Lock()
				delete(l.evaluating, item.ID)
				l.mu.Unlock()
				l.notify()
			}()
			l.Evaluate(ctx, item)
		}(actx, item)
	}
}

// rebaseRequested claims every item in the served orgs whose rebase is
// requested and that no rebase here is on, most urgent goal first across
// the orgs (rebaseOrder; the goals are read only for an org with one),
// and starts a session of kind rebase on each claim it wins. Not gated
// by capacity: a rebase finishes work already done. A refusal — another
// node holds it, or it is no longer requested — is the ordinary answer
// and is not logged as an error.
func (l *Loop) rebaseRequested(ctx context.Context) {
	if l.Rebase == nil {
		return
	}
	var requested []readyItem
	for _, sc := range l.Scopes {
		rows, err := l.GW.RequestedRebases(ctx, sc.ID)
		if err != nil {
			l.logf("list requested rebases of %s: %v", sc.Login, err)
			continue
		}
		if len(rows) == 0 {
			continue
		}
		rank := l.goalRanks(ctx, sc, "rebased")
		for _, it := range rows {
			requested = append(requested, readyItem{item: it, scope: sc, rank: rank(it.GoalID)})
		}
	}
	rebaseOrder(requested)
	for _, r := range requested {
		item, sc := r.item, r.scope
		l.mu.Lock()
		if l.rebasing == nil {
			l.rebasing = map[string]bool{}
		}
		if l.rebasing[item.ID] {
			l.mu.Unlock()
			continue
		}
		l.mu.Unlock()
		actx, ok := l.accountToFinish(ctx, priorityName(r.rank), "rebase")
		if !ok {
			continue
		}
		l.mu.Lock()
		l.rebasing[item.ID] = true
		l.mu.Unlock()
		// the claim is the lock across nodes: refused, another node has it
		if err := l.GW.ClaimWorkItemRebase(ctx, item.ID, sc.NodeID); err != nil {
			l.mu.Lock()
			delete(l.rebasing, item.ID)
			l.mu.Unlock()
			if !gateway.IsRefusal(err) {
				l.logf("%s: claim the rebase of item %d: %v", sc.Login, item.Seq, err)
			}
			continue
		}
		l.logf("%s: rebasing item %d %q", sc.Login, item.Seq, item.Title)
		l.wg.Add(1)
		go func(ctx context.Context, item gateway.WorkItem) {
			defer l.wg.Done()
			defer func() {
				l.mu.Lock()
				delete(l.rebasing, item.ID)
				l.mu.Unlock()
				l.notify()
			}()
			l.Rebase(ctx, item)
		}(actx, item)
	}
}

// planRequested starts the planner on every goal in planning in the
// served orgs that no planner here is on, most urgent goal first across
// the orgs (planOrder), each on the account chosen for its priority. A
// goal no account has headroom for is left for later.
func (l *Loop) planRequested(ctx context.Context) {
	if l.Plan == nil {
		return
	}
	var requested []planningGoal
	for _, sc := range l.Scopes {
		goals, err := l.goals(ctx, sc)
		if err != nil {
			l.logf("list goals of %s: %v", sc.Login, err)
			continue
		}
		for _, g := range goals {
			if g.Status == "planning" {
				requested = append(requested, planningGoal{goal: g, scope: sc})
			}
		}
	}
	planOrder(requested)
	for _, r := range requested {
		g, sc := r.goal, r.scope
		l.mu.Lock()
		if l.planning == nil {
			l.planning = map[string]bool{}
		}
		if l.planning[g.ID] {
			l.mu.Unlock()
			continue
		}
		l.mu.Unlock()
		actx, ok := l.accountFor(ctx, priorityName(gateway.PriorityRank(g.Priority)), true, "plan")
		if !ok {
			continue
		}
		l.mu.Lock()
		l.planning[g.ID] = true
		l.mu.Unlock()
		// the claim is the lock across nodes: refused, another node has it
		if err := l.GW.ClaimPlan(ctx, g.ID, sc.NodeID); err != nil {
			l.mu.Lock()
			delete(l.planning, g.ID)
			l.mu.Unlock()
			if gateway.IsRefusal(err) {
				l.logf("%s: goal %q is being planned elsewhere", sc.Login, g.Slug)
			} else {
				l.logf("%s: claim planning of %q: %v", sc.Login, g.Slug, err)
			}
			continue
		}
		l.logf("%s: planning goal %q", sc.Login, g.Slug)
		l.wg.Add(1)
		go func(ctx context.Context, g gateway.Goal, sc Scope) {
			defer l.wg.Done()
			defer func() {
				l.mu.Lock()
				delete(l.planning, g.ID)
				l.mu.Unlock()
				l.notify()
			}()
			l.Plan(ctx, g, sc)
		}(actx, g, sc)
	}
}

// researchDue offers every live direction of the served orgs to the
// platform: claiming is asking, and being refused is being told to wait
// — the window in the direction's own zone, the gap, and another node's
// claim are all the platform's to judge (runsheet trd §6c), so a
// refusal is not logged as an error. Each direction is offered on its
// own, so two directions of one project take their turns independently.
func (l *Loop) researchDue(ctx context.Context) {
	if l.Research == nil {
		return
	}
	for _, sc := range l.Scopes {
		directions, err := l.GW.Directions(ctx, sc.ID, "")
		if err != nil {
			l.logf("list directions of %s: %v", sc.Login, err)
			continue
		}
		live := make([]gateway.Direction, 0, len(directions))
		for _, d := range directions {
			if d.Active() {
				live = append(live, d)
			}
		}
		if len(live) == 0 {
			continue
		}
		projects, err := l.GW.Projects(ctx, sc.ID)
		if err != nil {
			l.logf("list projects of %s: %v", sc.Login, err)
			continue
		}
		byID := make(map[string]gateway.Project, len(projects))
		for _, p := range projects {
			byID[p.ID] = p
		}
		// the org's goals once, to see which directions have room: a
		// session on a project at its ceiling would have every proposal
		// dropped, and an unattended night should not pay for that
		goals, err := l.goals(ctx, sc)
		if err != nil {
			l.logf("list goals of %s: %v", sc.Login, err)
			continue
		}
		for _, d := range live {
			p, ok := byID[d.ProjectID]
			if !ok {
				continue // archived, or not this node's to see
			}
			if RoomFor(d, p, goals) <= 0 {
				continue
			}
			// research is not work that waits: it runs only on a
			// subscription with headroom for normal work, never on an
			// api account and never on a paced one
			actx, ok := l.accountFor(ctx, "low", false, "")
			if !ok {
				continue
			}
			l.mu.Lock()
			if l.researching == nil {
				l.researching = map[string]bool{}
			}
			if l.researching[d.ID] {
				l.mu.Unlock()
				continue
			}
			l.researching[d.ID] = true
			l.mu.Unlock()
			if err := l.GW.ClaimDirectionResearch(ctx, d.ID, sc.NodeID); err != nil {
				l.mu.Lock()
				delete(l.researching, d.ID)
				l.mu.Unlock()
				if !gateway.IsRefusal(err) {
					l.logf("%s: claim the research of %q: %v", sc.Login, p.Slug+"/"+d.Slug, err)
				}
				continue
			}
			l.logf("%s: researching %q", sc.Login, p.Slug+"/"+d.Slug)
			l.wg.Add(1)
			go func(ctx context.Context, d gateway.Direction, p gateway.Project, sc Scope) {
				defer l.wg.Done()
				defer func() {
					l.mu.Lock()
					delete(l.researching, d.ID)
					l.mu.Unlock()
					l.notify()
				}()
				l.Research(ctx, d, p, sc)
			}(actx, d, p, sc)
		}
	}
}

// Adopt runs an item this node already holds on the platform (claimed
// or running, after a restart) without claiming it again. It reports
// whether the item was taken up (false when it is already running here,
// or the node is draining — the node after it adopts the item).
func (l *Loop) Adopt(ctx context.Context, item gateway.WorkItem) bool {
	l.mu.Lock()
	if l.running == nil {
		l.running = map[string]bool{}
	}
	if l.running[item.ID] || l.draining {
		l.mu.Unlock()
		return false
	}
	l.running[item.ID] = true
	l.mu.Unlock()
	l.logf("adopted held item %d %q", item.Seq, item.Title)
	// already held: it runs on the account with the most headroom, or on
	// the first listed when none has any (OpenAccount's fallback)
	if actx, ok := l.accountFor(ctx, "high", true, ""); ok {
		ctx = actx
	}
	l.run(ctx, item)
	return true
}

func (l *Loop) start(ctx context.Context, item gateway.WorkItem) {
	l.mu.Lock()
	l.running[item.ID] = true
	l.mu.Unlock()
	l.run(ctx, item)
}

// run dispatches an item already marked running.
func (l *Loop) run(ctx context.Context, item gateway.WorkItem) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer func() {
			l.mu.Lock()
			delete(l.running, item.ID)
			l.mu.Unlock()
			if l.Release != nil {
				l.Release(item)
			}
			l.notify()
		}()
		if l.Dispatch != nil {
			l.Dispatch(ctx, item)
		}
	}()
}
