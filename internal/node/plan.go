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
	// Accounts are the node's Claude accounts (node.yaml); a session runs
	// on the first, or on the operator's own login when there are none.
	// They are the fallback to the credentials the platform issues.
	Accounts []AccountConfig
	// Credentials asks the platform for the session's credentials and
	// reports its usage record (nil: node.yaml's accounts, no record).
	Credentials *Credentials
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

// projectRepos reads the goal's project and its enrolled repos, by
// owner/name.
func (p *Planner) projectRepos(ctx context.Context, goal gateway.Goal, scope Scope) (*gateway.Project, map[string]gateway.Repo, error) {
	project, err := p.GW.Project(ctx, goal.ProjectID)
	if err != nil {
		return nil, nil, fmt.Errorf("project: %w", err)
	}
	all, err := p.GW.Repos(ctx, scope.AccountID, scope.Login)
	if err != nil {
		return nil, nil, fmt.Errorf("repos: %w", err)
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
		return nil, nil, fmt.Errorf("project %s has no repos enrolled; nothing to plan in", project.Slug)
	}
	return project, repos, nil
}

func (p *Planner) plan(ctx context.Context, goal gateway.Goal, scope Scope) error {
	project, repos, err := p.projectRepos(ctx, goal, scope)
	if err != nil {
		return err
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
	items, reasons, flags, cost, acct, err := p.ask(ctx, goal, scope.NodeID, dir, budget, in, repos, merged)
	// the session's usage report rides the plan's hand-in; Finish logs it
	// when nothing carried it (its report was built as it ended, in ask)
	defer acct.Finish(ctx, harness.Result{})
	if err != nil {
		return err
	}
	if len(flags) > 0 {
		// adr-004: an acceptance no evaluator can see is the plan's error;
		// the planner is asked once more with what was flagged
		p.logf("goal %s: plan names what no evaluator can see; asking again: %s", goal.Slug, strings.Join(flags, " | "))
		in.Rejected = flags
		var again float64
		var second *SessionAccount
		items, reasons, flags, again, second, err = p.ask(ctx, goal, scope.NodeID, dir, budget, in, repos, merged)
		defer second.Finish(ctx, harness.Result{})
		// one hand-in, one report: the first session's rides the second's
		second.Carries(acct)
		if second != nil {
			acct = second
		}
		cost += again
		if err != nil {
			return err
		}
		if len(flags) > 0 {
			// adr-004: the second plan is handed in as it stands; the
			// evaluator's unverifiable-clause rule is the backstop
			p.logf("goal %s: plan still names what no evaluator can see; handing it in: %s", goal.Slug, strings.Join(flags, " | "))
		}
	}
	return p.handIn(ctx, goal, items, reasons, cost, acct)
}

// handIn hands the graph in with PlanGoal, carrying the planner
// session's usage report (acct's, built as it ended; none for an adopted
// one), and logs each item's model.
func (p *Planner) handIn(ctx context.Context, goal gateway.Goal, items []gateway.PlannedItem, reasons map[int]string, cost float64, acct *SessionAccount) error {
	by := ""
	if p.Cfg != nil {
		by = p.Cfg.OwnerID
	}
	if err := acct.Carried(p.GW.PlanGoal(ctx, goal.ID, by, items, Cents(cost), acct.Report(harness.Result{}))); err != nil {
		return fmt.Errorf("hand in the plan: %w", err)
	}
	p.logf("goal %s: planned, %d items ($%.2f)", goal.Slug, len(items), cost)
	for _, it := range items {
		model := it.Model
		if model == "" {
			model = "none (the node's own)"
		}
		if r := reasons[it.Seq]; r != "" {
			model += ": " + r
		}
		p.logf("goal %s: item %d %q: model %s", goal.Slug, it.Seq, it.Title, model)
	}
	return nil
}

// ask runs one planner session on in and reads its graph: the items
// mapped onto the project's repos, each item's model_reason by seq, the
// check's flags on their acceptance (unverifiableAcceptance), the
// session's cost and its account, whose usage report is built as the
// session ends: the caller hands it in and Finishes it. An error means
// there is no graph to hand in.
func (p *Planner) ask(ctx context.Context, goal gateway.Goal, nodeID, dir string, budget float64, in harness.PlanInput, repos map[string]gateway.Repo, merged []int) ([]gateway.PlannedItem, map[int]string, []string, float64, *SessionAccount, error) {
	job := harness.Job{Kind: harness.KindPlan, Workspace: dir, Prompt: harness.PlanPrompt(in), MaxBudgetUSD: budget, Label: goal.Slug + " plan", Timeout: p.Timeout, MCPServers: p.MCP, Model: p.Model}
	acct, err := p.Credentials.Open(ctx, gateway.SessionRef{Kind: string(harness.KindPlan), GoalID: goal.ID}, nodeID, p.Accounts, Adapter(p.Harness))
	if err != nil {
		return nil, nil, nil, 0, nil, err
	}
	defer unlessDetached(ctx, acct.Cleanup)
	acct.Apply(&job)
	job.Meta = harness.SessionMeta{NodeID: nodeID, GoalID: goal.ID, TempDirs: sessionTempDirs(job, acct)}
	// the session outlives this process: a restart adopts it (ADR 009)
	job.Outlive = true
	result, err := p.Harness.Run(ctx, job, nil)
	acct.Report(result)
	items, reasons, flags, cost, err := p.readPlan(ctx, goal, dir, acct, result, err, repos, merged)
	return items, reasons, flags, cost, acct, err
}

// readPlan is what follows a planner session, however this process came
// to hold it: its graph read and mapped onto the project's repos, as ask
// describes. A cancelled ctx leaves the session running for the next
// node to adopt; otherwise its session directory goes.
func (p *Planner) readPlan(ctx context.Context, goal gateway.Goal, dir string, acct *SessionAccount, result harness.Result, err error, repos map[string]gateway.Repo, merged []int) ([]gateway.PlannedItem, map[int]string, []string, float64, error) {
	if ctx.Err() != nil {
		return nil, nil, nil, 0, ctx.Err()
	}
	defer harness.RemoveSession(dir)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, nil, nil, 0, fmt.Errorf("planner session ran out of time (%s)", p.Timeout)
	}
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("planner session: %w", acct.RedactErr(err))
	}
	result.Text = acct.Redact(result.Text)
	specs, err := harness.ParsePlan(result.Text, merged...)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("planner session (%s): %w: %s", result.SessionID, err, truncate(result.Text, 300))
	}
	items := make([]gateway.PlannedItem, 0, len(specs))
	itemRepo := map[int]gateway.Repo{}
	reasons := map[int]string{}
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
			return nil, nil, nil, 0, fmt.Errorf("item %d names repo %q, not one of the project's", sp.Seq, sp.Repo)
		}
		itemRepo[sp.Seq] = r
		base := strings.TrimSpace(sp.BaseRef)
		if base == "" {
			base = r.DefaultBranch
		}
		deps, because := p.explainedDeps(goal.Slug, sp)
		model := strings.TrimSpace(sp.Model)
		if model != "" && !PlannableModel(model) {
			// the planner's choice is advice: an unknown one leaves the
			// item on the node's own model rather than failing the plan
			p.logf("goal %s: item %d %q: model %q is not haiku, sonnet, opus or a claude-* id; dropped", goal.Slug, sp.Seq, strings.TrimSpace(sp.Title), model)
			model = ""
		}
		reasons[sp.Seq] = strings.TrimSpace(sp.ModelReason)
		items = append(items, gateway.PlannedItem{Seq: sp.Seq, Title: strings.TrimSpace(sp.Title), Brief: briefWithDeps(sp.Brief, deps, because), Acceptance: strings.TrimSpace(sp.Acceptance), DependsOn: deps, DependsBecause: because, RepoID: r.ID, BaseRef: base, Model: model})
	}
	return items, reasons, unverifiableAcceptance(specs, itemRepo, repos), result.CostUSD, nil
}

// explainedDeps keeps the item's dependencies that carry a because and
// drops the rest, logging each (trd §5 phase 2, the dependency rubric):
// the kept seqs and, by seq, what each one uses.
func (p *Planner) explainedDeps(goal string, sp harness.PlannedSpec) ([]int, map[int]string) {
	deps := []int{}
	var because map[int]string
	for _, d := range sp.DependsOn {
		why := strings.TrimSpace(d.Because)
		if why == "" {
			p.logf("plan %s: item %d dropped dependency on %d: no because", goal, sp.Seq, d.Seq)
			continue
		}
		if because == nil {
			because = map[int]string{}
		}
		if _, dup := because[d.Seq]; dup {
			continue
		}
		deps = append(deps, d.Seq)
		because[d.Seq] = why
	}
	return deps, because
}

// briefWithDeps is the item's brief with a line per kept dependency,
// so the builder sees what it is waiting for.
func briefWithDeps(brief string, deps []int, because map[int]string) string {
	brief = strings.TrimSpace(brief)
	if len(deps) == 0 {
		return brief
	}
	var b strings.Builder
	b.WriteString(brief)
	if brief != "" {
		b.WriteString("\n\n")
	}
	for i, d := range deps {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Depends on #%d because %s", d, strings.TrimRight(because[d], "."))
		b.WriteString(".")
	}
	return b.String()
}

// AdoptPlan takes up a planner session a node process before this one
// started (ADR 009): followed to its end, or read from its file when it
// is gone, its graph read and handed in. A plan the session left flagged
// (adr-004) is handed in as it stands: its prompt is not kept to ask
// again. A goal no longer in planning is not its to plan: the session is
// stopped and its directory removed.
func (p *Planner) AdoptPlan(ctx context.Context, goal gateway.Goal, scope Scope, s FoundSession) error {
	if goal.Status != "planning" {
		p.logf("goal %s: adopted plan is no longer wanted (%s); stopped", goal.Slug, goal.Status)
		s.Stop()
		return harness.RemoveSession(s.State.Workspace)
	}
	_, repos, err := p.projectRepos(ctx, goal, scope)
	if err != nil {
		return err
	}
	var merged []int
	if items, err := p.GW.GoalWorkItems(ctx, goal.ID); err == nil {
		for _, it := range items {
			if it.Status == "merged" {
				merged = append(merged, it.Seq)
			}
		}
	}
	at, ok := p.Harness.(harness.Attacher)
	if !ok {
		s.Stop()
		harness.RemoveSession(s.State.Workspace)
		return errNoAttach
	}
	p.logf("goal %s: adopted its %s planner session (pid %d)", goal.Slug, s.Liveness(), s.State.PID)
	result, err := at.Attach(ctx, s.Dir, nil)
	items, reasons, flags, cost, err := p.readPlan(ctx, goal, s.State.Workspace, nil, result, err, repos, merged)
	if err != nil {
		return err
	}
	if len(flags) > 0 {
		p.logf("goal %s: adopted plan names what no evaluator can see; handing it in: %s", goal.Slug, strings.Join(flags, " | "))
	}
	return p.handIn(ctx, goal, items, reasons, cost, nil)
}
