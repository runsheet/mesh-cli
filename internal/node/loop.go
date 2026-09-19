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

	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
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
		l.mu.Lock()
		l.running[item.ID] = true
		l.mu.Unlock()
		l.wg.Add(1)
		go func(item gateway.WorkItem) {
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
		}(item)
	}
}
