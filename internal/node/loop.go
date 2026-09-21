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
	GW       Gateway
	Cfg      *Config
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
	// AfterTick runs at the end of every tick (optional).
	AfterTick func(ctx context.Context)

	mu         sync.Mutex
	running    map[string]bool
	evaluating map[string]bool
	wg         sync.WaitGroup
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
	if err := l.GW.HeartbeatNode(ctx, l.Cfg.NodeID, l.Version, l.Running()); err != nil {
		l.logf("heartbeat: %v", err)
	}
	l.evaluateHeld(ctx)
	free := l.Cfg.Capacity - l.Running()
	if free <= 0 {
		return
	}
	items, err := l.GW.ReadyWorkItems(ctx, l.Cfg.OrgID)
	if err != nil {
		l.logf("list ready items: %v", err)
		return
	}
	for _, item := range items {
		if free <= 0 {
			return
		}
		l.mu.Lock()
		already := l.running[item.ID]
		l.mu.Unlock()
		if already {
			continue
		}
		if err := l.GW.ClaimWorkItem(ctx, item.ID, l.Cfg.NodeID, l.Harness); err != nil {
			if gateway.IsRefusal(err) {
				l.logf("item %d %q: not ours (%v)", item.Seq, item.Title, err)
			} else {
				l.logf("claim item %d: %v", item.Seq, err)
			}
			continue
		}
		free--
		l.logf("claimed item %d %q", item.Seq, item.Title)
		l.start(ctx, item)
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
	items, err := l.GW.OrgWorkItems(ctx, l.Cfg.OrgID, "evaluating")
	if err != nil {
		l.logf("list evaluating items: %v", err)
		return
	}
	for _, item := range items {
		if item.NodeID != l.Cfg.NodeID {
			continue
		}
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
