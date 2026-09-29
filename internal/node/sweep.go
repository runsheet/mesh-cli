package node

import (
	"context"
	"errors"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// SweepScopes is the start-up sweep (mesh-cli ADR 008, The start-up
// sweep): every active mesh-session scope the user manager runs that no
// found session is about to adopt is looked up on the platform by its
// kind and subject, and one whose subject is terminal there — or unknown
// to it — is killed with everything in its cgroup, each process named in
// the log. A live subject, a research scope, a name that does not parse
// and a platform that cannot answer all leave the scope: an outage is
// not a leak. It returns how many scopes it stopped.
func SweepScopes(ctx context.Context, gw Gateway, sc sessionunit.Systemctl, found []FoundSession, logf func(string, ...any)) (stopped int) {
	scopes, err := sc.ListScopes()
	if err != nil {
		logf("sweep: list scopes: %v", err)
		return 0
	}
	adopting := map[string]bool{}
	for _, s := range found {
		if s.State.Unit != "" {
			adopting[strings.TrimSuffix(s.State.Unit, ".scope")] = true
		}
	}
	for _, scope := range scopes {
		unit := strings.TrimSuffix(scope.Unit, ".scope")
		if scope.Active == "inactive" || scope.Active == "failed" || adopting[unit] {
			continue
		}
		j := judgeScope(ctx, gw, unit)
		switch j.Verdict {
		case ScopeUnparsed:
			logf("sweep: %s left, its name names no session", unit)
			continue
		case ScopeUnread:
			logf("sweep: %s left, %s %s unread: %v", unit, j.What, j.Subject, j.Err)
			continue
		case ScopeResearch:
			logf("sweep: %s left, research is not swept (a direction needs its project to be read)", unit)
			continue
		case ScopeStrange:
			logf("sweep: %s left, kind %q is not one the node runs", unit, j.Kind)
			continue
		case ScopeLive:
			logf("sweep: %s left, %s %s is %s on the platform", unit, j.What, j.Subject, j.Status)
			continue
		}
		what, subject, status := j.What, j.Subject, j.Status
		killed, err := sessionunit.SystemdScope{Systemctl: sc}.Kill(sessionunit.Handle{UnitName: unit})
		names := make([]string, 0, len(killed))
		for _, p := range killed {
			names = append(names, p.String())
		}
		list := strings.Join(names, ", ")
		if list == "" {
			list = "nothing"
		}
		if err != nil {
			logf("sweep: stopping %s (%s %s, %s on the platform): killed %s: %v", unit, what, subject, status, list, err)
			continue
		}
		logf("sweep: stopped %s (%s %s, %s on the platform): killed %s", unit, what, subject, status, list)
		stopped++
	}
	return stopped
}

// A ScopeVerdict is what the platform's answer makes of a mesh-session
// scope: the sweep stops a terminal or unknown one and leaves the rest,
// and `mesh node status` counts the live ones as live and the terminal
// and unknown ones as leaked.
type ScopeVerdict string

const (
	ScopeLive     ScopeVerdict = "live"
	ScopeTerminal ScopeVerdict = "terminal"
	ScopeUnknown  ScopeVerdict = "unknown"
	ScopeResearch ScopeVerdict = "research"
	ScopeUnparsed ScopeVerdict = "unparsed"
	ScopeStrange  ScopeVerdict = "strange"
	ScopeUnread   ScopeVerdict = "unread"
	ScopeInactive ScopeVerdict = "inactive"
)

// scopeJudgement is one scope's kind and subject, as its name says them,
// and the platform's answer.
type scopeJudgement struct {
	Kind, Subject string
	// What is "item" or "goal", by the kind.
	What string
	// Status is the subject's status on the platform ("unknown" for an
	// id it has no row for).
	Status  string
	Verdict ScopeVerdict
	Err     error
}

// judgeScope parses unit's name and asks the platform after its
// subject: a build, evaluate or rebase scope's item, a plan scope's
// goal.
func judgeScope(ctx context.Context, gw Gateway, unit string) scopeJudgement {
	kind, subject, ok := sessionunit.ParseUnitName(unit)
	if !ok {
		return scopeJudgement{Verdict: ScopeUnparsed}
	}
	j := scopeJudgement{Kind: kind, Subject: subject}
	var err error
	live := false
	switch harness.Kind(kind) {
	case harness.KindBuild, harness.KindEvaluate, harness.KindRebase:
		j.What = "item"
		var item *gateway.WorkItem
		if item, err = gw.WorkItem(ctx, subject); err == nil {
			j.Status, live = itemStatus(*item), SessionLive(*item)
		}
	case harness.KindPlan:
		j.What = "goal"
		var goal *gateway.Goal
		if goal, err = gw.Goal(ctx, subject); err == nil {
			j.Status, live = goal.Status, GoalLive(*goal)
		}
	case harness.KindResearch:
		j.Verdict = ScopeResearch
		return j
	default:
		j.Verdict = ScopeStrange
		return j
	}
	switch {
	case errors.Is(err, gateway.ErrNotFound):
		j.Status, j.Verdict = "unknown", ScopeUnknown
	case err != nil:
		j.Verdict, j.Err = ScopeUnread, err
	case live:
		j.Verdict = ScopeLive
	default:
		j.Verdict = ScopeTerminal
	}
	return j
}

// ScopeRow is one line of `mesh node status`'s sessions listing.
type ScopeRow struct {
	// Unit is the scope's name without ".scope".
	Unit string
	// Active is its ActiveState.
	Active string
	// Kind and Subject are as its name says them; empty when it does
	// not parse.
	Kind, Subject string
	// What is "item" or "goal"; Status its status on the platform.
	What, Status string
	Verdict      ScopeVerdict
	// Err is why the platform did not answer (ScopeUnread).
	Err error
}

// ScopeCounts is the listing's `sessions:` line: scopes of sessions the
// platform says are live, and scopes of terminal or unknown subjects.
// Research, unparsed, unread and inactive scopes are neither.
type ScopeCounts struct{ Live, Leaked int }

// ScopeReport lists this machine's mesh-session scopes against the
// platform (mesh-cli ADR 008, The start-up sweep): each with its kind,
// subject and the platform's answer, judged as the sweep judges it, and
// the counts. An inactive or failed scope is listed but not looked up.
func ScopeReport(ctx context.Context, gw Gateway, sc sessionunit.Systemctl) ([]ScopeRow, ScopeCounts, error) {
	scopes, err := sc.ListScopes()
	if err != nil {
		return nil, ScopeCounts{}, err
	}
	var rows []ScopeRow
	var n ScopeCounts
	for _, scope := range scopes {
		unit := strings.TrimSuffix(scope.Unit, ".scope")
		row := ScopeRow{Unit: unit, Active: scope.Active}
		if scope.Active == "inactive" || scope.Active == "failed" {
			row.Kind, row.Subject, _ = sessionunit.ParseUnitName(unit)
			row.Verdict = ScopeInactive
			rows = append(rows, row)
			continue
		}
		j := judgeScope(ctx, gw, unit)
		row.Kind, row.Subject, row.What, row.Status, row.Verdict, row.Err = j.Kind, j.Subject, j.What, j.Status, j.Verdict, j.Err
		switch j.Verdict {
		case ScopeLive:
			n.Live++
		case ScopeTerminal, ScopeUnknown:
			n.Leaked++
		}
		rows = append(rows, row)
	}
	return rows, n, nil
}

// SessionLive is whether an item is in a status a node session runs
// under: claimed or running (a build), evaluating (an evaluation), or a
// rebase claimed (runsheet ADR 008). Every other — ready, pending,
// review, merged, failed, cancelled, done, already_done — is terminal
// for a session: none should be running on it.
func SessionLive(item gateway.WorkItem) bool {
	switch item.Status {
	case "claimed", "running", "evaluating":
		return true
	}
	return item.RebaseState == "claimed"
}

// GoalLive is SessionLive for a plan: a goal is planned only while it
// is planning.
func GoalLive(goal gateway.Goal) bool { return goal.Status == "planning" }

// itemStatus is the item's status as the sweep's log line says it, its
// rebase named when one is claimed.
func itemStatus(item gateway.WorkItem) string {
	if item.RebaseState == "claimed" {
		return item.Status + " (rebase claimed)"
	}
	return item.Status
}
