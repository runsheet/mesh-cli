package node

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"

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
	// MaxBudgetUSD caps the planning session (5 by default).
	MaxBudgetUSD float64
	Log          *log.Logger

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
	files, err := WriteContext(dir, cx.Docs)
	if err != nil {
		return fmt.Errorf("write context: %w", err)
	}
	contextDir := ""
	if len(files) > 0 {
		contextDir = ContextDir
	}
	prompt := harness.PlanPrompt(harness.PlanInput{
		GoalSlug: goal.Slug, GoalTitle: goal.Title, Statement: goal.Statement, Acceptance: goal.Acceptance, BudgetCents: goal.BudgetCents,
		Org: scope.Login, Project: project.Slug, Repos: dirs, ContextDir: contextDir, ContextFiles: files, Blocked: goal.PausedReason,
	})
	budget := p.MaxBudgetUSD
	if budget <= 0 {
		budget = 5
	}
	job := harness.Job{Kind: harness.KindPlan, Workspace: dir, Prompt: prompt, MaxBudgetUSD: budget, Label: goal.Slug + " plan"}
	p.logf("goal %s: planning in %s (%d repos)", goal.Slug, dir, len(repos))
	result, err := p.Harness.Run(ctx, job, nil)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("planner session: %w", err)
	}
	specs, err := harness.ParsePlan(result.Text)
	if err != nil {
		return fmt.Errorf("planner session (%s): %w: %s", result.SessionID, err, truncate(result.Text, 300))
	}
	items := make([]gateway.PlannedItem, 0, len(specs))
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
			return fmt.Errorf("item %d names repo %q, not one of the project's", sp.Seq, sp.Repo)
		}
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
	by := ""
	if p.Cfg != nil {
		by = p.Cfg.OwnerID
	}
	if err := p.GW.PlanGoal(ctx, goal.ID, by, items, Cents(result.CostUSD)); err != nil {
		return fmt.Errorf("hand in the plan: %w", err)
	}
	p.logf("goal %s: planned, %d items ($%.2f)", goal.Slug, len(items), result.CostUSD)
	return nil
}
