package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Planner is the session that compiles a goal into its graph (mesh-specs
// prd, the planner; trd §5 phase 2): the project's repos checked out
// read-only, its Prologue context, the statement and the acceptance; a
// fresh `claude -p` that reads and then writes the items as JSON; the
// graph handed in with PlanGoal, the session's cost with it. A goal
// whose plan cannot be read three times is left in planning and said so.
type Planner struct {
	GW       Gateway
	Harness  harness.Harness
	WS       Workspacer
	Cfg      *Config
	WorkRoot string
	// MCP are the MCP servers the session gets (Prologue).
	MCP map[string]any
	// MaxBudgetUSD caps the planning session (5 by default).
	MaxBudgetUSD float64
	// Model is what a planner session runs on ("" the CLI's default).
	Model string
	// Timeout caps its wall clock (0 none).
	Timeout time.Duration
	Log     *log.Logger

	mu       sync.Mutex
	attempts map[string]int
}

// MaxPlanAttempts is how many unreadable plans a goal gets before the
// planner stops trying (until the node restarts).
const MaxPlanAttempts = 3

func (p *Planner) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log.Printf(format, args...)
	}
}

// PlanDir is where a goal's planner works: <root>/<goal-slug>/plan.
func PlanDir(root, goalSlug string) string { return filepath.Join(root, goalSlug, "plan") }

// Plan compiles one goal in planning and hands the graph in. An error
// means nothing was handed in.
func (p *Planner) Plan(ctx context.Context, goal gateway.Goal, scope Scope) error {
	p.mu.Lock()
	if p.attempts == nil {
		p.attempts = map[string]int{}
	}
	tries := p.attempts[goal.ID]
	p.mu.Unlock()
	if tries >= MaxPlanAttempts {
		return nil // said so already
	}
	err := p.plan(ctx, goal, scope)
	if err != nil && ctx.Err() == nil {
		p.mu.Lock()
		p.attempts[goal.ID] = tries + 1
		p.mu.Unlock()
		if tries+1 >= MaxPlanAttempts {
			p.logf("goal %s: no plan after %d tries; leaving it in planning (restart the node to try again): %v", goal.Slug, tries+1, err)
		}
	}
	return err
}

func (p *Planner) plan(ctx context.Context, goal gateway.Goal, scope Scope) error {
	project, err := p.GW.Project(ctx, goal.ProjectID)
	if err != nil {
		return fmt.Errorf("project: %w", err)
	}
	all, err := p.GW.Repos(ctx, scope.AccountID, scope.Login)
	if err != nil {
		return fmt.Errorf("repos: %w", err)
	}
	inProject := map[string]bool{}
	for _, id := range project.RepoIDs {
		inProject[id] = true
	}
	repos := map[string]gateway.Repo{} // owner/name → repo
	for _, r := range all {
		if inProject[r.ID] {
			repos[r.FullName()] = r
		}
	}
	if len(repos) == 0 {
		return fmt.Errorf("project %s has no repos enrolled; nothing to plan in", project.Slug)
	}
	dir := PlanDir(p.WorkRoot, goal.Slug)
	dirs := map[string]string{}
	for name, r := range repos {
		sub := filepath.Join("repos", r.Name)
		if err := p.WS.Fetch(ctx, name, filepath.Join(dir, sub)); err != nil {
			return fmt.Errorf("fetch %s: %w", name, err)
		}
		dirs[name] = sub
	}
	cx, err := FetchContext(ctx, p.GW, scope.Login, project.Slug)
	if err != nil {
		p.logf("goal %s: context unavailable: %v", goal.Slug, err)
		cx = &Context{}
	}
	AddLive(ctx, p.GW, cx, scope.ID, project.ID, p.logf)
	files, err := WriteContext(dir, cx)
	if err != nil {
		return fmt.Errorf("write context: %w", err)
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 || len(cx.Live) > 0 {
		contextDir = ContextDir
	}
	// a re-plan: the items so far, and where the numbering goes on
	var existing []harness.ExistingItem
	var merged []int
	nextSeq := 1
	if items, err := p.GW.GoalWorkItems(ctx, goal.ID); err == nil {
		for _, it := range items {
			note := it.Evaluation
			if it.LastError != "" {
				note = it.LastError
			}
			existing = append(existing, harness.ExistingItem{Seq: it.Seq, Title: it.Title, Status: it.Status, Note: truncate(note, 300), Branch: it.Branch, PRURL: it.PRURL})
			if it.Status == "merged" {
				merged = append(merged, it.Seq)
			}
			if it.Seq >= nextSeq {
				nextSeq = it.Seq + 1
			}
		}
	} else {
		p.logf("goal %s: items so far unavailable: %v", goal.Slug, err)
	}
	in := harness.PlanInput{
		GoalSlug: goal.Slug, GoalTitle: goal.Title, Statement: goal.Statement, Acceptance: goal.Acceptance, BudgetCents: goal.BudgetCents,
		Org: scope.Login, Project: project.Slug, Repos: dirs, ContextDir: contextDir, ContextFiles: files, Blocked: goal.PausedReason,
		Existing: existing, NextSeq: nextSeq, Prologue: len(p.MCP) > 0,
	}
	budget := p.MaxBudgetUSD
	if budget <= 0 {
		budget = 5
	}
	p.logf("goal %s: planning in %s (%d repos)", goal.Slug, dir, len(repos))
	items, flags, cost, err := p.ask(ctx, goal, dir, budget, in, repos, merged)
	if err != nil {
		return err
	}
	if len(flags) > 0 {
		// adr-004: an acceptance no evaluator can see is the plan's error;
		// the planner is asked once more with what was flagged
		p.logf("goal %s: plan names what no evaluator can see; asking again: %s", goal.Slug, strings.Join(flags, " | "))
		in.Rejected = flags
		var again float64
		items, flags, again, err = p.ask(ctx, goal, dir, budget, in, repos, merged)
		cost += again
		if err != nil {
			return err
		}
		if len(flags) > 0 {
			return fmt.Errorf("plan still names what no evaluator can see after the re-ask ($%.2f spent): %s", cost, strings.Join(flags, " | "))
		}
	}
	by := ""
	if p.Cfg != nil {
		by = p.Cfg.OwnerID
	}
	if err := p.GW.PlanGoal(ctx, goal.ID, by, items, Cents(cost)); err != nil {
		return fmt.Errorf("hand in the plan: %w", err)
	}
	p.logf("goal %s: planned, %d items ($%.2f)", goal.Slug, len(items), cost)
	return nil
}

// ask runs one planner session on in and reads its graph: the items
// mapped onto the project's repos, the check's flags on their acceptance
// (unverifiableAcceptance), and the session's cost. An error means there
// is no graph to hand in.
func (p *Planner) ask(ctx context.Context, goal gateway.Goal, dir string, budget float64, in harness.PlanInput, repos map[string]gateway.Repo, merged []int) ([]gateway.PlannedItem, []string, float64, error) {
	job := harness.Job{Kind: harness.KindPlan, Workspace: dir, Prompt: harness.PlanPrompt(in), MaxBudgetUSD: budget, Label: goal.Slug + " plan", Timeout: p.Timeout, MCPServers: p.MCP, Model: p.Model}
	result, err := p.Harness.Run(ctx, job, nil)
	if ctx.Err() != nil {
		return nil, nil, 0, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, nil, 0, fmt.Errorf("planner session ran out of time (%s)", p.Timeout)
	}
	if err != nil {
		return nil, nil, 0, fmt.Errorf("planner session: %w", err)
	}
	specs, err := harness.ParsePlan(result.Text, merged...)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("planner session (%s): %w: %s", result.SessionID, err, truncate(result.Text, 300))
	}
	items := make([]gateway.PlannedItem, 0, len(specs))
	itemRepo := map[int]gateway.Repo{}
	for _, sp := range specs {
		r, ok := repos[sp.Repo]
		if !ok {
			// a bare name, when it is one of the project's
			for name, cand := range repos {
				if strings.EqualFold(cand.Name, sp.Repo) || strings.EqualFold(name, sp.Repo) {
					r, ok = cand, true
				}
			}
		}
		if !ok {
			return nil, nil, 0, fmt.Errorf("item %d names repo %q, not one of the project's", sp.Seq, sp.Repo)
		}
		itemRepo[sp.Seq] = r
		base := strings.TrimSpace(sp.BaseRef)
		if base == "" {
			base = r.DefaultBranch
		}
		deps := sp.DependsOn
		if deps == nil {
			deps = []int{}
		}
		items = append(items, gateway.PlannedItem{Seq: sp.Seq, Title: strings.TrimSpace(sp.Title), Brief: strings.TrimSpace(sp.Brief), Acceptance: strings.TrimSpace(sp.Acceptance), DependsOn: deps, RepoID: r.ID, BaseRef: base})
	}
	return items, unverifiableAcceptance(specs, itemRepo, repos), result.CostUSD, nil
}
