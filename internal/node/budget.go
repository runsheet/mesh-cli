package node

import (
	"fmt"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// defaultBudgets are node.yaml `budgets:`'s figures for a kind of
// session and a model tier when the file names none (ADR 012): the
// usage ledger's ten costliest sessions were runaway sonnet builds at
// or near the old flat $10 cap, so only "build" gets defaults of its
// own; the other kinds keep their historical flat fallback until
// node.yaml, or the platform, names one for them too.
var defaultBudgets = map[string]map[string]float64{
	"build": {"haiku": 2, "sonnet": 4, "opus": 10},
}

// ModelTier is the budget tier a model resolves to — haiku, sonnet or
// opus — matched the way ModelRank is: a full id such as
// "claude-sonnet-5" by its family name. "" when m names none of the
// three (empty, or a Codex model).
func ModelTier(m string) string {
	m = strings.ToLower(m)
	switch {
	case strings.Contains(m, "opus"):
		return "opus"
	case strings.Contains(m, "sonnet"):
		return "sonnet"
	case strings.Contains(m, "haiku"):
		return "haiku"
	}
	return ""
}

// kindModelBudget is node.yaml's (or, live, Loop.KindModelBudget's)
// `budgets:` figure for a kind of session and a model, resolved to its
// tier by ModelTier, and where it came from for the session's starting
// log line: the map's own entry, or the kind's default (build only,
// ADR 012). 0 and "" when neither names one — the caller's own
// fallback wins instead.
func kindModelBudget(budgets map[string]map[string]float64, kind, model string) (amount float64, source string) {
	tier := ModelTier(model)
	if tier == "" {
		return 0, ""
	}
	if v, ok := budgets[kind][tier]; ok && v > 0 {
		return v, fmt.Sprintf("node.yaml budgets.%s.%s", kind, tier)
	}
	if v, ok := defaultBudgets[kind][tier]; ok {
		return v, fmt.Sprintf("the default for %s/%s", kind, tier)
	}
	return 0, ""
}

// KindModelBudget is node.yaml's `budgets:` figure for a kind of
// session and a model (kindModelBudget, ADR 012).
//
// The platform is meant to override this per claimed item or project
// once schema.json carries a `budgets` field (mesh-spends-less-per-item
// item 2's brief); it does not yet, so node.yaml is the only source.
func (c *Config) KindModelBudget(kind, model string) (amount float64, source string) {
	if c == nil {
		return kindModelBudget(nil, kind, model)
	}
	return kindModelBudget(c.Budgets, kind, model)
}

// KindModelBudget is node.yaml's `budgets:` in force — as last
// reloaded, else Cfg's — for a kind of session and a model: read live
// like the accounts and project budgets (Spending, ADR 012).
func (l *Loop) KindModelBudget(kind, model string) (amount float64, source string) {
	return kindModelBudget(l.sessionBudgets(), kind, model)
}

// minPositive is the smallest of vs greater than 0; 0 when none is —
// "zero means the other one wins", generalized to any count of values.
func minPositive(vs ...float64) float64 {
	best := 0.0
	for _, v := range vs {
		if v > 0 && (best == 0 || v < best) {
			best = v
		}
	}
	return best
}

// SessionBudget is a session's cost cap (ADR 012): the smallest of the
// goal's own remaining budget (when it names one), kindBudget — node.yaml's
// (or the platform's, once it carries the field) `budgets:` figure for
// the session's kind and model, kindSource where it came from — and
// fallback, the caller's own traditional default when neither of the
// other two applies. It also names where the winning figure came from,
// for the session's starting log line.
func SessionBudget(kindBudget float64, kindSource string, goal *gateway.Goal, fallback float64) (amount float64, source string) {
	remaining := 0.0
	if goal != nil && goal.BudgetCents > 0 {
		remaining = max(0, float64(goal.BudgetCents-goal.SpentCents)/100)
	}
	amount = minPositive(remaining, kindBudget, fallback)
	switch {
	case amount == 0:
		return 0, "no cap"
	case amount == remaining:
		return amount, "the goal's remaining budget"
	case amount == kindBudget:
		return amount, kindSource
	default:
		return amount, "the session's own cap"
	}
}
