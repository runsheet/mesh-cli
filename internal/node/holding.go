package node

import (
	"fmt"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Unclaimed is one goal's ready work the node passed over this pass for
// lack of account headroom — the loop's own accounting (Loop.passOver
// fills it in, one per goal and kind; HoldingReport only folds it into
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

	out.Accounts = holdingAccounts(now, accounts, readings, cfg, capacity)
	for _, a := range out.Accounts {
		if a.Status != "exhausted" {
			allExhausted = false
		}
		holds(a.ResetsAt)
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

// holdingAccounts is each account's row of the report: its status by
// pace on its latest reading (ok, paced, exhausted, or unknown with
// none), when it resumes and what the node still takes on it.
func holdingAccounts(now time.Time, accounts []AccountConfig, readings map[string]harness.RateLimit, cfg LimitsConfig, capacity int) []gateway.HoldingAccount {
	out := []gateway.HoldingAccount{}
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
		out = append(out, gateway.HoldingAccount{
			Account: a.Label, Kind: holdingKind(a.Kind), Status: status, ResetsAt: resetsAt,
			Takes: holdingTakes(status, a.Kind, capacity),
		})
	}
	return out
}

// heldReason is an Unclaimed's reason: one sentence naming the goal's
// priority and what held it — held when the loop says (the urgent-only
// reserve, a ranged node at its min), else every account's standing
// from its row of the report, e.g. "normal priority; only urgent and
// high while paced (mesh - max20 until 2026-09-28T18:00:00Z); claude -
// shhemail exhausted until 2026-10-03T03:00:00Z".
func heldReason(priority, held string, rows []gateway.HoldingAccount) string {
	parts := []string{priority + " priority"}
	if held != "" {
		parts = append(parts, held)
	}
	for _, a := range rows {
		until := ""
		if !a.ResetsAt.IsZero() {
			until = " until " + a.ResetsAt.UTC().Format(time.RFC3339)
		}
		switch a.Status {
		case "paced":
			parts = append(parts, fmt.Sprintf("only urgent and high while paced (%s%s)", a.Account, until))
		case "exhausted":
			parts = append(parts, fmt.Sprintf("%s exhausted%s", a.Account, until))
		case "ok":
			if held == "" {
				// headroom by its window, but no room by its spending:
				// an api key at its cap or its reserve, or no slot free
				parts = append(parts, a.Account+" has no room for it (its cap, its reserve or its slots)")
			}
		}
	}
	return strings.Join(parts, "; ")
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
