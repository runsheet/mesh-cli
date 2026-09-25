package node

import (
	"context"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// ModelRank orders the model tiers a session may run on, weakest first:
// haiku 1, sonnet 2, opus 3. A full id (claude-opus-5-5, claude-sonnet-5)
// ranks by its family; "" and anything unknown rank 0, below all three.
func ModelRank(m string) int {
	m = strings.ToLower(m)
	switch {
	case strings.Contains(m, "opus"):
		return 3
	case strings.Contains(m, "sonnet"):
		return 2
	case strings.Contains(m, "haiku"):
		return 1
	}
	return 0
}

// Stronger is the higher-ranked of a and b; a when they rank the same.
func Stronger(a, b string) string {
	if ModelRank(b) > ModelRank(a) {
		return b
	}
	return a
}

// Strongest is the highest-ranked of ms, the first of those that tie;
// "" when there are none.
func Strongest(ms ...string) string {
	best := ""
	for i, m := range ms {
		if i == 0 {
			best = m
			continue
		}
		best = Stronger(best, m)
	}
	return best
}

// PlannableModel reports whether m is a model a plan may name for an
// item: one of the aliases haiku, sonnet and opus, or a full claude-* id.
func PlannableModel(m string) bool {
	m = strings.ToLower(strings.TrimSpace(m))
	switch m {
	case "haiku", "sonnet", "opus":
		return true
	}
	return strings.HasPrefix(m, "claude-") && len(m) > len("claude-")
}

// ItemModel is the model the item itself asks to be built on (runsheet
// trd §6a, items-carry-a-model): the plan's Model, else the model of
// the agent it names — the most specific definition that has one
// (WorkItem.AgentIDs). "" when it asks for none: an item from an older
// platform, one with no agent, or an agent that names no model or
// cannot be read (the caller then falls back to the node's own).
func ItemModel(ctx context.Context, gw Gateway, item gateway.WorkItem) string {
	if item.Model != "" {
		return item.Model
	}
	for _, id := range item.AgentIDs() {
		m, err := gw.AgentModel(ctx, id)
		if err != nil {
			return ""
		}
		if m != "" {
			return m
		}
	}
	return ""
}

// BuildModel is what a session that works on the item's branch — the
// build, and a rebase once the node runs one — runs on: the item's
// model, then its agent's, then nodeDefault (node.yaml build/default;
// "" the CLI's own).
func BuildModel(ctx context.Context, gw Gateway, item gateway.WorkItem, nodeDefault string) string {
	if m := ItemModel(ctx, gw, item); m != "" {
		return m
	}
	return nodeDefault
}

// EvaluateModel is what the item's evaluator runs on: never below the
// builder and never below sonnet — the strongest of the node's own
// evaluate model, the item's build model, the tier the platform chose
// to evaluate it on, and sonnet. An item that asks for no model (an
// older platform) evaluates on nodeDefault as it always has.
func EvaluateModel(ctx context.Context, gw Gateway, item gateway.WorkItem, nodeDefault string) string {
	build := ItemModel(ctx, gw, item)
	if build == "" && item.EvaluateModel == "" {
		return nodeDefault
	}
	// nodeDefault first: of two that tie, the operator's own id wins
	return Strongest(nodeDefault, build, item.EvaluateModel, "sonnet")
}

// modelName is how a log line or a note names a session's model.
func modelName(m string) string {
	if m == "" {
		return "the CLI's default model"
	}
	return m
}
