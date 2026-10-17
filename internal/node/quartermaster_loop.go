package node

import (
	"context"
	"os"
	"sync"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// quartermasterState is what the Loop keeps of its Quartermaster's runs:
// the latest findings, for every heartbeat, and what it last logged of
// each non-ok one, so a finding is logged once while it is unchanged.
type quartermasterState struct {
	mu       sync.Mutex
	findings []Finding
	// logged is each non-ok finding's severity and detail as last
	// logged, by kind and subject.
	logged map[string]string
	// unsent is whether the platform's lack of heartbeat findings has
	// been said (once per run).
	unsent bool
}

// runQuartermaster runs l.Quartermaster when it is due (its Every since
// the last run): the findings are told to l.Findings and kept for the
// heartbeat, each non-ok one logged `quartermaster: <kind> <severity>
// <subject>: <detail>` once while it is unchanged, and each reclaim
// `quartermaster: reclaimed <bytes> (<kind>)`.
func (l *Loop) runQuartermaster(ctx context.Context) {
	if l.Quartermaster == nil {
		return
	}
	findings, ran := l.Quartermaster.RunIfDue(ctx, l.now())
	if !ran {
		return
	}
	q := &l.quartermaster
	q.mu.Lock()
	q.findings = append([]Finding(nil), findings...)
	if q.logged == nil {
		q.logged = map[string]string{}
	}
	seen := map[string]bool{}
	for _, f := range findings {
		if f.Reclaimed > 0 {
			l.logf("quartermaster: reclaimed %s (%s)", humanBytes(f.Reclaimed), f.Kind)
		}
		key := f.Kind + "\x00" + f.Subject
		if f.Severity == "ok" || f.Severity == "" {
			continue
		}
		seen[key] = true
		said := f.Severity + "\x00" + f.Detail
		if q.logged[key] == said {
			continue
		}
		q.logged[key] = said
		l.logf("quartermaster: %s %s %s: %s", f.Kind, f.Severity, f.Subject, f.Detail)
	}
	// a finding back to ok, or gone, is logged again when it next turns
	for key := range q.logged {
		if !seen[key] {
			delete(q.logged, key)
		}
	}
	q.mu.Unlock()
	if l.Findings != nil {
		l.Findings(findings)
	}
}

// LatestFindings is the quartermaster's latest findings, nil before its first
// run.
func (l *Loop) LatestFindings() []Finding {
	q := &l.quartermaster
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Finding(nil), q.findings...)
}

// heartbeatFindings is the heartbeat's `findings`: the latest findings
// as the gateway's shape. A platform whose heartbeat takes no
// `findings` gets none (gateway.Client.TakesHeartbeatFindings), said
// once per run.
func (l *Loop) heartbeatFindings(ctx context.Context) []gateway.Finding {
	findings := l.LatestFindings()
	if len(findings) == 0 {
		return nil
	}
	if p, ok := l.GW.(interface {
		TakesHeartbeatFindings(context.Context) bool
	}); ok && !p.TakesHeartbeatFindings(ctx) {
		q := &l.quartermaster
		q.mu.Lock()
		said := q.unsent
		q.unsent = true
		q.mu.Unlock()
		if !said {
			l.logf("platform has no heartbeat findings; kept on the node")
		}
		return nil
	}
	out := make([]gateway.Finding, 0, len(findings))
	for _, f := range findings {
		out = append(out, gateway.Finding{Kind: f.Kind, Severity: f.Severity, Subject: f.Subject, Detail: f.Detail, Bytes: f.Bytes, Reclaimed: f.Reclaimed, At: f.At})
	}
	return out
}

// LiveCheckout says whether a session of this node's may be using the
// checkout dir: its session directory (<dir>.session) is there — a
// session running, detached for the next process to adopt, or ended and
// not yet handled — or an adopted session's is. The quartermaster's
// dead-checkout check keeps such a checkout whatever its item's status.
func (l *Loop) LiveCheckout(dir string) bool {
	sessionDir := dir + ".session"
	if _, err := os.Stat(sessionDir); err == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.adopted {
		if e.found.Dir == sessionDir || e.found.Dir == dir {
			return true
		}
	}
	return false
}
