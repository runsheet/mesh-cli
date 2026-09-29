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
		kind, subject, ok := sessionunit.ParseUnitName(unit)
		if !ok {
			logf("sweep: %s left, its name names no session", unit)
			continue
		}
		var what, status string
		var live bool
		switch harness.Kind(kind) {
		case harness.KindBuild, harness.KindEvaluate, harness.KindRebase:
			what = "item"
			item, err := gw.WorkItem(ctx, subject)
			switch {
			case errors.Is(err, gateway.ErrNotFound):
				status = "unknown"
			case err != nil:
				logf("sweep: %s left, item %s unread: %v", unit, subject, err)
				continue
			default:
				status, live = itemStatus(*item), SessionLive(*item)
			}
		case harness.KindPlan:
			what = "goal"
			goal, err := gw.Goal(ctx, subject)
			switch {
			case errors.Is(err, gateway.ErrNotFound):
				status = "unknown"
			case err != nil:
				logf("sweep: %s left, goal %s unread: %v", unit, subject, err)
				continue
			default:
				status, live = goal.Status, GoalLive(*goal)
			}
		case harness.KindResearch:
			logf("sweep: %s left, research is not swept (a direction needs its project to be read)", unit)
			continue
		default:
			logf("sweep: %s left, kind %q is not one the node runs", unit, kind)
			continue
		}
		if live {
			logf("sweep: %s left, %s %s is %s on the platform", unit, what, subject, status)
			continue
		}
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
