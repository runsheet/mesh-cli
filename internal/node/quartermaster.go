package node

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Finding is one thing a quartermaster check found about the node's own
// fitness (goal the-quartermaster-keeps-the-nodes-fit): disk space,
// scratch left in /tmp, a dead checkout, a version drift — the kinds
// the goal's evidence named after dl360's disk filled on 2026-10-15 and
// the merge queue deadlocked.
type Finding struct {
	// Kind is disk, scratch, dead_checkout, version or kept_sessions.
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
// the-quartermaster-keeps-the-nodes-fit). The Loop runs it every tick
// (Loop.Quartermaster): node.findings and the heartbeat's `findings`
// carry what it found.
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
	findings, _ := q.RunIfDue(ctx, now)
	return findings
}

// RunIfDue is Run, saying too whether the checks ran this time (false:
// the last run's findings, inside Every).
func (q *Quartermaster) RunIfDue(ctx context.Context, now time.Time) ([]Finding, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.lastRun.IsZero() && now.Sub(q.lastRun) < q.every() {
		return q.last, false
	}
	q.lastRun = now
	var out []Finding
	for i, check := range q.Checks {
		out = append(out, runQuartermasterCheck(ctx, check, i, now)...)
	}
	q.last = out
	return out, true
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

// FindingsLines is `mesh node status`'s findings block: a `findings:`
// header, then one line per finding sorted critical first, then warn,
// then ok, and by kind within a severity — `  <kind> <severity>
// <subject>: <detail>` — or `findings: none` with none (goal
// the-quartermaster-keeps-the-nodes-fit).
func FindingsLines(findings []Finding) []string {
	if len(findings) == 0 {
		return []string{"findings: none"}
	}
	sorted := append([]Finding(nil), findings...)
	sort.SliceStable(sorted, func(i, j int) bool {
		si, sj := findingSeverityRank(sorted[i].Severity), findingSeverityRank(sorted[j].Severity)
		if si != sj {
			return si < sj
		}
		return sorted[i].Kind < sorted[j].Kind
	})
	out := []string{"findings:"}
	for _, f := range sorted {
		out = append(out, fmt.Sprintf("  %s %s %s: %s", f.Kind, f.Severity, f.Subject, f.Detail))
	}
	return out
}

// findingSeverityRank orders a Finding's severity critical first, then
// warn, then ok (and anything else).
func findingSeverityRank(severity string) int {
	switch severity {
	case "critical":
		return 0
	case "warn":
		return 1
	default:
		return 2
	}
}
