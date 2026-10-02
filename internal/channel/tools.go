package channel

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
)

// Handler owns the tools the session calls back with. Each is a thin
// call on the gateway as the node's operator — none touches a workspace
// or git; the daemon hears about finish/fail through OnDone so the
// claim's slot is released.
type Handler struct {
	GW  node.Gateway
	Cfg *node.Config
	// Scopes are the orgs the node serves: a goal slug is looked up in
	// each, an item's org names its mesh-context project.
	Scopes node.Scopes
	Log    *log.Logger
	// OnDone is told when an item's session reported finish or fail.
	OnDone func(itemID, outcome string)

	mu        sync.Mutex
	sessionID string
}

// SetSessionID records the claude session id the daemon observed, for
// progress reports.
func (h *Handler) SetSessionID(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if id != "" {
		h.sessionID = id
	}
}

func (h *Handler) session() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessionID
}

func (h *Handler) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log.Printf(format, args...)
	}
}

func str(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func num(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func (h *Handler) done(itemID, outcome string) {
	if h.OnDone != nil {
		h.OnDone(itemID, outcome)
	}
}

// Instructions is what the MCP server tells the session on connect.
const Instructions = `Events from the Mesh node arrive as <channel source="mesh" kind="..." ...>. ` +
	`kind="work_item.ready" carries item_id, seq, goal_slug, repo, base_ref, branch and workspace attributes and the item's brief and acceptance as content: ` +
	`it is claimed for you — do the work, then call mesh_finish (or mesh_fail). ` +
	`kind="goal.paused" / "goal.cancelled" / "goal.resumed" and kind="checkpoint" are notices: stop, drop or continue the goal's items accordingly; no reply is needed. ` +
	`Report with mesh_progress as you go, mesh_record after each milestone and before mesh_finish (it reports to the platform and writes no file), and answer a question with reply. Keep every message short.`

// Tools lists the server's tools over h.
func (h *Handler) Tools() []Tool {
	return []Tool{
		{Name: "mesh_progress", Description: "Report progress on a work item: a short note and the cents spent since the last report.",
			Schema: ObjectSchema(map[string]string{"item_id": "the work item id", "note": "what happened, one line", "cost_cents": "cents spent since the last report (0 if unknown)"}, "item_id", "note"),
			Call:   h.progress},
		{Name: "mesh_finish", Description: "The item's branch is pushed: record it and end the item. Push first. " +
			"If main already holds the item's work, finish with outcome already_done and the evidence instead: no branch.",
			Schema: ObjectSchema(map[string]string{"item_id": "the work item id", "branch": "the pushed branch (outcome built)", "head_sha": "its head commit (outcome built)",
				"outcome":  "built (the default: a branch was pushed) or already_done (main already holds the work)",
				"evidence": "for already_done: the merged PR URLs and/or commit SHAs on the base ref that satisfy the acceptance, with a line on why"}, "item_id"),
			Call: h.finish},
		{Name: "mesh_fail", Description: "The item cannot be done: record why and end it.",
			Schema: ObjectSchema(map[string]string{"item_id": "the work item id", "error": "what went wrong"}, "item_id", "error"),
			Call:   h.fail},
		{Name: "mesh_evaluate", Description: "Record an evaluation verdict on a finished item (the evaluator's).",
			Schema: ObjectSchema(map[string]string{"item_id": "the work item id", "passed": "whether the acceptance holds", "notes": "the review", "cost_cents": "cents the evaluation spent (0 if unknown)"}, "item_id", "passed"),
			Call:   h.evaluate},
		{Name: "mesh_context", Description: "The goal's context: the project's instructions, ADRs, PRD and TRD from mesh-context. Read it before working on an item.",
			Schema: ObjectSchema(map[string]string{"goal_slug": "the goal's slug", "org": "the goal's org login, when the node serves more than one"}, "goal_slug"),
			Call:   h.context},
		{Name: "mesh_items", Description: "List the work items of the orgs this node serves, optionally in one status (pending, ready, claimed, running, evaluating, review, merged, failed, cancelled).",
			Schema: ObjectSchema(map[string]string{"status": "only this status"}),
			Call:   h.items},
		{Name: "mesh_record", Description: "Record a milestone: the summary goes to the platform as a progress note on the item. Writes no file.",
			Schema: ObjectSchema(map[string]string{"item_id": "the work item id", "summary": "one line for the log"}, "item_id", "summary"),
			Call:   h.record},
		{Name: "reply", Description: "Acknowledge an event or answer the platform in a sentence; recorded as a progress note on the item when item_id is given.",
			Schema: ObjectSchema(map[string]string{"text": "the reply", "item_id": "the item it concerns, if any"}, "text"),
			Call:   h.reply},
	}
}

func (h *Handler) progress(ctx context.Context, args map[string]any) (string, error) {
	id := str(args, "item_id")
	if id == "" {
		return "", fmt.Errorf("item_id required")
	}
	if err := h.GW.ReportWorkItemProgress(ctx, id, str(args, "note"), num(args, "cost_cents"), h.session()); err != nil {
		return "", err
	}
	return "recorded", nil
}

func (h *Handler) finish(ctx context.Context, args map[string]any) (string, error) {
	id, branch := str(args, "item_id"), str(args, "branch")
	switch outcome := str(args, "outcome"); outcome {
	case "", gateway.OutcomeBuilt:
	case gateway.OutcomeAlreadyDone:
		evidence := str(args, "evidence")
		if id == "" || evidence == "" {
			return "", fmt.Errorf("item_id and evidence required for outcome already_done")
		}
		if err := h.GW.FinishWorkItemAlreadyDone(ctx, id, evidence, num(args, "cost_cents"), nil); err != nil {
			return "", err
		}
		h.done(id, "finished")
		return "finished: already done", nil
	default:
		return "", fmt.Errorf("outcome must be built or already_done, not %q", outcome)
	}
	if id == "" || branch == "" {
		return "", fmt.Errorf("item_id and branch required")
	}
	if err := h.GW.FinishWorkItem(ctx, id, branch, str(args, "head_sha"), num(args, "cost_cents"), nil); err != nil {
		return "", err
	}
	h.done(id, "finished")
	return "finished: " + branch, nil
}

func (h *Handler) fail(ctx context.Context, args map[string]any) (string, error) {
	id, msg := str(args, "item_id"), str(args, "error")
	if id == "" || msg == "" {
		return "", fmt.Errorf("item_id and error required")
	}
	if err := h.GW.FailWorkItem(ctx, id, msg); err != nil {
		return "", err
	}
	h.done(id, "failed")
	return "failed", nil
}

func (h *Handler) evaluate(ctx context.Context, args map[string]any) (string, error) {
	id := str(args, "item_id")
	passed, _ := args["passed"].(bool)
	if id == "" {
		return "", fmt.Errorf("item_id required")
	}
	if err := h.GW.EvaluateWorkItem(ctx, id, passed, str(args, "notes"), nil, num(args, "cost_cents"), nil); err != nil {
		return "", err
	}
	return fmt.Sprintf("evaluated: passed=%v", passed), nil
}

func (h *Handler) context(ctx context.Context, args map[string]any) (string, error) {
	slug := str(args, "goal_slug")
	if slug == "" {
		return "", fmt.Errorf("goal_slug required")
	}
	goal, scope, err := h.goalBySlug(ctx, slug, str(args, "org"))
	if err != nil {
		return "", err
	}
	project, err := h.GW.Project(ctx, goal.ProjectID)
	if err != nil {
		return "", err
	}
	cx, err := node.FetchContext(ctx, h.GW, scope.Name(), project.Slug, node.ContextOptions{})
	if err != nil {
		return "", err
	}
	if len(cx.Docs) == 0 && len(cx.Index) == 0 {
		return fmt.Sprintf("No context documents in project %s.", project.Slug), nil
	}
	names := make([]string, 0, len(cx.Docs))
	for n := range cx.Docs {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "# Context for goal %s (project %s)\n\n", slug, project.Slug)
	for _, n := range names {
		fmt.Fprintf(&b, "<!-- %s -->\n%s\n\n", n, strings.TrimSpace(cx.Docs[n]))
	}
	if len(cx.Index) > 0 {
		b.WriteString(node.RenderIndex(cx))
	}
	return b.String(), nil
}

// goalBySlug finds a goal by slug in the org named, else in each org
// the node serves (the id derives from (org, slug)).
func (h *Handler) goalBySlug(ctx context.Context, slug, org string) (*gateway.Goal, node.Scope, error) {
	for _, sc := range h.Scopes {
		if org != "" && !strings.EqualFold(org, sc.Name()) {
			continue
		}
		id, err := uuid.Parse(sc.ID)
		if err != nil {
			continue
		}
		goal, err := h.GW.Goal(ctx, gateway.GoalID(id, slug).String())
		if err == nil && goal != nil && goal.Slug == slug {
			return goal, sc, nil
		}
	}
	if org != "" {
		return nil, node.Scope{}, fmt.Errorf("no goal %q in %s", slug, org)
	}
	return nil, node.Scope{}, fmt.Errorf("no goal %q in %s", slug, strings.Join(h.Scopes.Names(), ", "))
}

func (h *Handler) items(ctx context.Context, args map[string]any) (string, error) {
	var items []gateway.WorkItem
	for _, sc := range h.Scopes {
		rows, err := h.GW.OrgWorkItems(ctx, sc.ID, str(args, "status"))
		if err != nil {
			return "", err
		}
		items = append(items, rows...)
	}
	if len(items) == 0 {
		return "no items", nil
	}
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "- %s · seq %d · %s · %s", it.ID, it.Seq, it.Status, it.Title)
		if len(h.Scopes) > 1 {
			if sc, ok := h.Scopes.ByOrg(it.OrgID); ok {
				fmt.Fprintf(&b, " · %s", sc.Name())
			}
		}
		if it.Branch != "" {
			fmt.Fprintf(&b, " · %s", it.Branch)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// record hands a milestone's summary to the platform as a progress
// note on the item. It writes no file and makes no commit: status lives
// on the platform, and the goal doc is written once, when the goal
// finishes (ADR 003, decision 4).
func (h *Handler) record(ctx context.Context, args map[string]any) (string, error) {
	id, summary := str(args, "item_id"), str(args, "summary")
	if id == "" || summary == "" {
		return "", fmt.Errorf("item_id and summary required")
	}
	if err := h.GW.ReportWorkItemProgress(ctx, id, summary, 0, h.session()); err != nil {
		return "", err
	}
	return "recorded on the platform", nil
}

func (h *Handler) reply(ctx context.Context, args map[string]any) (string, error) {
	text := str(args, "text")
	if text == "" {
		return "", fmt.Errorf("text required")
	}
	h.logf("session: %s", text)
	if id := str(args, "item_id"); id != "" {
		if err := h.GW.ReportWorkItemProgress(ctx, id, text, 0, h.session()); err != nil {
			return "", err
		}
		return "noted on the item", nil
	}
	return "noted", nil
}

// ReadyEvent renders the work_item.ready event for a claimed, resolved item.
func ReadyEvent(res *node.Resolved, workspace string) (string, map[string]string) {
	it := res.Item
	var b strings.Builder
	fmt.Fprintf(&b, "Work item %d of goal %q is claimed for you: %s\n\n", it.Seq, res.Goal.Title, it.Title)
	if strings.TrimSpace(it.Brief) != "" {
		fmt.Fprintf(&b, "Brief:\n%s\n\n", strings.TrimSpace(it.Brief))
	}
	if strings.TrimSpace(it.Acceptance) != "" {
		fmt.Fprintf(&b, "Acceptance:\n%s\n\n", strings.TrimSpace(it.Acceptance))
	}
	if strings.TrimSpace(res.Goal.Acceptance) != "" {
		fmt.Fprintf(&b, "The goal's acceptance:\n%s\n\n", strings.TrimSpace(res.Goal.Acceptance))
	}
	if it.Attempts > 0 && strings.TrimSpace(it.Evaluation) != "" {
		fmt.Fprintf(&b, "This is attempt %d: the branch carries the last attempt's commits and an independent evaluator rejected it. Fix what it found first:\n%s\n\n", it.Attempts+1, strings.TrimSpace(it.Evaluation))
	}
	fmt.Fprintf(&b, "Repo %s, branch %s from %s, workspace %s. Call mesh_context(goal_slug=%q) first.", res.Repo.FullName(), res.Branch(), res.BaseRef(), workspace, res.Goal.Slug)
	meta := map[string]string{
		"kind": "work_item.ready", "item_id": it.ID, "seq": fmt.Sprint(it.Seq), "goal_slug": res.Goal.Slug,
		"repo": res.Repo.FullName(), "base_ref": res.BaseRef(), "branch": res.Branch(), "workspace": workspace,
		"at": time.Now().UTC().Format(time.RFC3339),
	}
	return b.String(), meta
}

// GoalEvent renders a goal status notice.
func GoalEvent(kind string, goal gateway.Goal) (string, map[string]string) {
	content := fmt.Sprintf("Goal %q (%s) is now %s.", goal.Title, goal.Slug, goal.Status)
	switch kind {
	case "goal.paused":
		content += " Stop work on its items after the current step; do not push."
		if goal.PausedReason != "" {
			content += " Reason: " + goal.PausedReason
		}
	case "goal.cancelled":
		content += " Drop its items."
	case "goal.resumed":
		content += " Continue its items."
	case "checkpoint":
		content = fmt.Sprintf("Goal %q (%s) awaits a person's approval at a checkpoint; nothing to do until it is approved.", goal.Title, goal.Slug)
	}
	return content, map[string]string{"kind": kind, "goal_slug": goal.Slug, "status": goal.Status}
}
