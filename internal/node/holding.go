package node

import (
	"fmt"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Unclaimed is one goal's ready work the node passed over this pass for
// lack of account headroom — the loop's own accounting (item 2 of
// node-says-why-work-waits fills it in; HoldingReport only folds it into
// the report).
type Unclaimed struct {
	GoalID string
	Kind   string // "item" or "plan"
	Count  int
	Reason string
}

// HoldingReport is the node's holding decision as data (runsheet goal
// node-says-why-work-waits): each account's status, when its window
// resets and what the node still takes on it, and the summary judgement
// that the console shows in place of the node's log line. Pure: now and
// readings are the same the loop paces by (Limits.Readings), capacity is
// the slots in force, p is the node's own aggregate pacing (nodePace)
// and unclaimed is the ready work passed over this pass.
func HoldingReport(now time.Time, accounts []AccountConfig, readings map[string]harness.RateLimit, cfg LimitsConfig, capacity int, p pacing, unclaimed []Unclaimed) gateway.Holding {
	out := gateway.Holding{
		Mode:      p.mode.String(),
		Accounts:  []gateway.HoldingAccount{},
		Unclaimed: []gateway.HoldingUnclaimed{},
	}

	allExhausted := len(accounts) > 0
	var until time.Time
	holds := func(resetsAt time.Time) {
		if resetsAt.IsZero() {
			return
		}
		if until.IsZero() || resetsAt.Before(until) {
			until = resetsAt
		}
	}

	for _, a := range accounts {
		status := "unknown"
		var resetsAt time.Time
		if r, seen := readings[a.Label]; seen {
			ap := pace(now, r, cfg)
			switch ap.mode {
			case paceNormal:
				status = "ok"
			case pacePaced:
				status = "paced"
			case paceStopped:
				status = "exhausted"
			}
			resetsAt = ap.resumeAt
		}
		if status != "exhausted" {
			allExhausted = false
		}
		holds(resetsAt)
		out.Accounts = append(out.Accounts, gateway.HoldingAccount{
			Account: a.Label, Kind: holdingKind(a.Kind), Status: status, ResetsAt: resetsAt,
			Takes: holdingTakes(status, a.Kind, capacity),
		})
	}
	out.Until = until

	var items, plans int
	for _, u := range unclaimed {
		switch u.Kind {
		case "item":
			items += u.Count
		case "plan":
			plans += u.Count
		}
		out.Unclaimed = append(out.Unclaimed, gateway.HoldingUnclaimed{
			GoalID: u.GoalID, Kind: u.Kind, Count: u.Count, Reason: u.Reason,
		})
	}

	switch {
	case allExhausted:
		out.Summary = fmt.Sprintf("no headroom: nothing claimed until %s", until.Format(time.RFC3339))
	case p.mode == paceStopped:
		out.Summary = fmt.Sprintf("stopped: nothing new until %s: %s", until.Format(time.RFC3339), p.why)
	case p.mode == pacePaced:
		out.Summary = fmt.Sprintf("paced: urgent and high only, %d item(s), no research", max(1, capacity/2))
	default:
		out.Summary = fmt.Sprintf("normal: up to %d item(s)", capacity)
	}
	if len(unclaimed) > 0 {
		out.Summary += fmt.Sprintf("; %d item(s) and %d plan(s) left unclaimed", items, plans)
	}
	return out
}

// holdingKind renders an AccountConfig.Kind as the heartbeat's holding
// shape names it: "subscription" or "api_key" (unlike Scaling's
// AccountScaling, which keeps AccountAPI's own "api").
func holdingKind(kind string) string {
	if kind == AccountAPI {
		return "api_key"
	}
	return "subscription"
}

// holdingTakes is what the node still takes on an account of that
// status: ok and unknown (never read yet, so ChooseAccount treats it as
// full headroom) take everything; paced only urgent and high, at most
// half the capacity, no research; exhausted takes nothing.
func holdingTakes(status, kind string, capacity int) gateway.HoldingTakes {
	switch status {
	case "paced":
		return gateway.HoldingTakes{Priorities: []string{"urgent", "high"}, MaxItems: max(1, capacity/2)}
	case "exhausted":
		return gateway.HoldingTakes{Priorities: []string{}}
	default: // ok, unknown
		return gateway.HoldingTakes{
			Priorities: []string{"urgent", "high", "normal", "low"},
			MaxItems:   capacity,
			Research:   kind == AccountSubscription,
		}
	}
}
