package node

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Finding is one thing a quartermaster check found about the node's own
// fitness (goal the-quartermaster-keeps-the-nodes-fit): disk space,
// scratch left in /tmp, a dead checkout, a version drift — the kinds
// the goal's evidence named after dl360's disk filled on 2026-10-15 and
// the merge queue deadlocked.
type Finding struct {
	// Kind is disk, scratch, dead_checkout or version.
	Kind string `json:"kind"`
	// Severity is ok, warn or critical.
	Severity  string    `json:"severity"`
	Subject   string    `json:"subject"`
	Detail    string    `json:"detail"`
	Bytes     int64     `json:"bytes,omitempty"`
	Reclaimed int64     `json:"reclaimed,omitempty"`
	At        time.Time `json:"at"`
}

// Check is one of the quartermaster's checks: it runs at now and
// returns what it found, zero or more Findings.
type Check func(ctx context.Context, now time.Time) []Finding

// DefaultQuartermasterEvery is how often Run actually runs the checks
// when Every is zero.
const DefaultQuartermasterEvery = 10 * time.Minute

// Quartermaster runs the node's own fitness checks periodically (goal
// the-quartermaster-keeps-the-nodes-fit). Not yet wired into the Loop
// or the heartbeat — that is item 5 of the same goal.
type Quartermaster struct {
	Checks []Check
	// Every is how often Run actually runs the checks; a call inside
	// Every of the last one returns what that run found, the checks
	// not run again. Zero means DefaultQuartermasterEvery.
	Every time.Duration

	mu      sync.Mutex
	lastRun time.Time
	last    []Finding
}

func (q *Quartermaster) every() time.Duration {
	if q.Every > 0 {
		return q.Every
	}
	return DefaultQuartermasterEvery
}

// Run runs every check at now and returns every finding, a panicking
// check caught and turned into one Finding naming it (never stopping
// the rest) — unless the last Run was inside Every, when it returns
// that run's findings without running the checks again.
func (q *Quartermaster) Run(ctx context.Context, now time.Time) []Finding {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.lastRun.IsZero() && now.Sub(q.lastRun) < q.every() {
		return q.last
	}
	q.lastRun = now
	var out []Finding
	for i, check := range q.Checks {
		out = append(out, runQuartermasterCheck(ctx, check, i, now)...)
	}
	q.last = out
	return out
}

// runQuartermasterCheck runs one check, recovering a panic into one
// Finding naming the check rather than letting it stop the others.
func runQuartermasterCheck(ctx context.Context, check Check, i int, now time.Time) (findings []Finding) {
	defer func() {
		if r := recover(); r != nil {
			findings = []Finding{{
				Kind:     "quartermaster",
				Severity: "critical",
				Subject:  fmt.Sprintf("check %d", i),
				Detail:   fmt.Sprintf("panicked: %v", r),
				At:       now,
			}}
		}
	}()
	if check == nil {
		return nil
	}
	return check(ctx, now)
}
