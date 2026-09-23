package node

import (
	"context"
	"log"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Loop is trd §3: every Interval, heartbeat; list the org's ready items;
// claim up to capacity − running; hand each claim to Dispatch. The claim
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
	// AfterTick runs at the end of every tick (optional).
	AfterTick func(ctx context.Context)

	mu          sync.Mutex
	running     map[string]bool
	evaluating  map[string]bool
	planning    map[string]bool
	researching map[string]bool
	wg          sync.WaitGroup
}

func (l *Loop) logf(format string, args ...any) {
	if l.Log != nil {
		l.Log.Printf(format, args...)
	}
}

// Running is the number of items this node is running.
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
// harnesses are killed by the same ctx).
func (l *Loop) Run(ctx context.Context) error {
	if l.running == nil {
		l.running = map[string]bool{}
	}
	interval := l.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	l.Tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.wg.Wait()
			return ctx.Err()
		case <-t.C:
			l.Tick(ctx)
		}
	}
}

// Tick is one round: heartbeat, list, claim, dispatch.
func (l *Loop) Tick(ctx context.Context) {
	if l.running == nil {
		l.running = map[string]bool{}
	}
	for _, sc := range l.Scopes {
		if err := l.GW.HeartbeatNode(ctx, sc.NodeID, l.Version, l.Running()); err != nil {
			l.logf("heartbeat %s: %v", sc.Login, err)
		}
	}
	l.evaluateHeld(ctx)
	l.planRequested(ctx)
	l.researchDue(ctx)
	free := l.Cfg.Capacity - l.Running()
	for _, sc := range l.Scopes {
		if free <= 0 {
			break
		}
		items, err := l.GW.ReadyWorkItems(ctx, sc.ID)
		if err != nil {
			l.logf("list ready items of %s: %v", sc.Login, err)
			continue
		}
		for _, item := range items {
			if free <= 0 {
				break
			}
			l.mu.Lock()
			already := l.running[item.ID]
			l.mu.Unlock()
			if already {
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
			l.start(ctx, item)
		}
	}
	if l.AfterTick != nil {
		l.AfterTick(ctx)
	}
}

// evaluateHeld starts an evaluation of every item this node finished
// that no evaluation here is running on. The verdict moves the item on;
// an evaluation that ends without one leaves it here for the next tick.
func (l *Loop) evaluateHeld(ctx context.Context) {
	if l.Evaluate == nil {
		return
	}
	var items []gateway.WorkItem
	for _, sc := range l.Scopes {
		rows, err := l.GW.OrgWorkItems(ctx, sc.ID, "evaluating")
		if err != nil {
			l.logf("list evaluating items of %s: %v", sc.Login, err)
			continue
		}
		for _, it := range rows {
			if it.NodeID == sc.NodeID {
				items = append(items, it)
			}
		}
	}
	for _, item := range items {
		l.mu.Lock()
		if l.evaluating == nil {
			l.evaluating = map[string]bool{}
		}
		if l.evaluating[item.ID] {
			l.mu.Unlock()
			continue
		}
		l.evaluating[item.ID] = true
		l.mu.Unlock()
		l.logf("evaluating item %d %q", item.Seq, item.Title)
		l.wg.Add(1)
		go func(item gateway.WorkItem) {
			defer l.wg.Done()
			defer func() {
				l.mu.Lock()
				delete(l.evaluating, item.ID)
				l.mu.Unlock()
			}()
			l.Evaluate(ctx, item)
		}(item)
	}
}

// planRequested starts the planner on every goal in planning in the
// served orgs that no planner here is on.
func (l *Loop) planRequested(ctx context.Context) {
	if l.Plan == nil {
		return
	}
	for _, sc := range l.Scopes {
		goals, err := l.GW.Goals(ctx, sc.ID)
		if err != nil {
			l.logf("list goals of %s: %v", sc.Login, err)
			continue
		}
		for _, g := range goals {
			if g.Status != "planning" {
				continue
			}
			l.mu.Lock()
			if l.planning == nil {
				l.planning = map[string]bool{}
			}
			if l.planning[g.ID] {
				l.mu.Unlock()
				continue
			}
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
			go func(g gateway.Goal, sc Scope) {
				defer l.wg.Done()
				defer func() {
					l.mu.Lock()
					delete(l.planning, g.ID)
					l.mu.Unlock()
				}()
				l.Plan(ctx, g, sc)
			}(g, sc)
		}
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
		goals, err := l.GW.Goals(ctx, sc.ID)
		if err != nil {
			l.logf("list goals of %s: %v", sc.Login, err)
			continue
		}
		for _, d := range live {
			p, ok := byID[d.ProjectID]
			if !ok {
				continue // archived, or not this node's to see
			}
			if RoomFor(d, goals) <= 0 {
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
			go func(d gateway.Direction, p gateway.Project, sc Scope) {
				defer l.wg.Done()
				defer func() {
					l.mu.Lock()
					delete(l.researching, d.ID)
					l.mu.Unlock()
				}()
				l.Research(ctx, d, p, sc)
			}(d, p, sc)
		}
	}
}

// Adopt runs an item this node already holds on the platform (claimed
// or running, after a restart) without claiming it again. It reports
// whether the item was taken up (false when it is already running here).
func (l *Loop) Adopt(ctx context.Context, item gateway.WorkItem) bool {
	l.mu.Lock()
	if l.running == nil {
		l.running = map[string]bool{}
	}
	if l.running[item.ID] {
		l.mu.Unlock()
		return false
	}
	l.mu.Unlock()
	l.logf("adopted held item %d %q", item.Seq, item.Title)
	l.start(ctx, item)
	return true
}

func (l *Loop) start(ctx context.Context, item gateway.WorkItem) {
	l.mu.Lock()
	l.running[item.ID] = true
	l.mu.Unlock()
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
		}()
		if l.Dispatch != nil {
			l.Dispatch(ctx, item)
		}
	}()
}
