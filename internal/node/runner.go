package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Resolved is a work item with its goal, project and repo read.
type Resolved struct {
	Item    gateway.WorkItem
	Goal    *gateway.Goal
	Project *gateway.Project
	Repo    *gateway.Repo
}

// Branch is the item's branch name.
func (r *Resolved) Branch() string { return BranchName(r.Goal.Slug, r.Item.Seq) }

// BaseRef is the item's base ref, or the repo's default branch.
func (r *Resolved) BaseRef() string {
	if r.Item.BaseRef != "" {
		return r.Item.BaseRef
	}
	if r.Repo != nil && r.Repo.DefaultBranch != "" {
		return r.Repo.DefaultBranch
	}
	return "main"
}

// Resolve reads the item's goal, project and repo.
func Resolve(ctx context.Context, gw Gateway, item gateway.WorkItem) (*Resolved, error) {
	goal, err := gw.Goal(ctx, item.GoalID)
	if err != nil {
		return nil, fmt.Errorf("goal: %w", err)
	}
	project, err := gw.Project(ctx, goal.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}
	repo, err := gw.Repo(ctx, item.RepoID)
	if err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}
	return &Resolved{Item: item, Goal: goal, Project: project, Repo: repo}, nil
}

// Runner runs one claimed item to its end (poll mode: `claude -p`).
type Runner struct {
	GW       Gateway
	Harness  harness.Harness
	WS       Workspacer
	Cfg      *Config
	Scopes   Scopes
	WorkRoot string
	// MCP are the MCP servers every session gets (Prologue).
	MCP map[string]any
	// MCPFunc, when set, is asked for them at every item instead: the
	// node's machine access token expires within the hour, so a build
	// started long after the node did must carry a bearer taken now
	// (channel.Session.MCPFunc is the same seam).
	MCPFunc func(context.Context) map[string]any
	// MaxBudgetUSD caps a session's budget whatever the goal has left.
	MaxBudgetUSD float64
	// Model is what a build session runs on ("" the CLI's default).
	Model string
	// ProgressEvery is how often progress is reported (60 s).
	ProgressEvery time.Duration
	Log           *log.Logger
}

// mcp is the session's MCP servers, asked for afresh per item when
// MCPFunc is set.
func (r *Runner) mcp(ctx context.Context) map[string]any {
	if r.MCPFunc != nil {
		return r.MCPFunc(ctx)
	}
	return r.MCP
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Printf(format, args...)
	}
}

// Budget is the session's cap: the goal's remaining budget in dollars,
// capped by MaxBudgetUSD (either 0 means "the other one").
func (r *Runner) Budget(goal *gateway.Goal) float64 {
	remaining := 0.0
	if goal.BudgetCents > 0 {
		remaining = math.Max(0, float64(goal.BudgetCents-goal.SpentCents)/100)
	}
	switch {
	case remaining > 0 && r.MaxBudgetUSD > 0:
		return math.Min(remaining, r.MaxBudgetUSD)
	case remaining > 0:
		return remaining
	default:
		return r.MaxBudgetUSD
	}
}

// costReporter turns cumulative dollars into the cents-since-last-report
// each ReportWorkItemProgress and FinishWorkItem carries.
type costReporter struct {
	mu            sync.Mutex
	reportedCents int
	totalUSD      float64
}

func (c *costReporter) observe(usd float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if usd > c.totalUSD {
		c.totalUSD = usd
	}
}

// delta returns the unreported cents and marks them reported.
func (c *costReporter) delta() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := int(math.Round(c.totalUSD * 100))
	d := total - c.reportedCents
	if d < 0 {
		d = 0
	}
	c.reportedCents = total
	return d
}

// RunItem is the whole of one claimed item: resolve, workspace, context,
// session, progress, push, finish — or fail. A cancelled ctx (the node's
// own shutdown) leaves the item as it is for a retry.
func (r *Runner) RunItem(ctx context.Context, item gateway.WorkItem) error {
	res, err := Resolve(ctx, r.GW, item)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("resolve: %w", err))
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	r.logf("item %s: %q on %s from %s", label, item.Title, res.Repo.FullName(), res.BaseRef())

	spec := WorkspaceSpec{Root: r.WorkRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: res.Branch(), BaseRef: res.BaseRef()}
	dir, err := r.WS.Prepare(ctx, spec)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("workspace: %w", err))
	}

	orgLogin := r.Scopes.LoginOf(item.OrgID)
	cx, err := FetchContext(ctx, r.GW, orgLogin, res.Project.Slug)
	if err != nil {
		r.logf("item %s: context unavailable: %v", label, err)
		cx = &Context{}
	}
	files, err := WriteContext(dir, cx)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("write context: %w", err))
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 {
		contextDir = ContextDir
	}

	mcp := r.mcp(ctx)
	prompt := harness.BuildPrompt(harness.PromptInput{
		GoalSlug: res.Goal.Slug, GoalTitle: res.Goal.Title, GoalStatement: res.Goal.Statement, GoalAcceptance: res.Goal.Acceptance,
		Seq: item.Seq, Title: item.Title, Brief: item.Brief, Acceptance: item.Acceptance,
		Repo: res.Repo.FullName(), Branch: spec.Branch, BaseRef: spec.BaseRef, ContextDir: contextDir, ContextFiles: files,
		Attempts: item.Attempts, Evaluation: item.Evaluation, Project: orgLogin + "/" + res.Project.Slug, Prologue: len(mcp) > 0,
	})
	job := harness.Job{Kind: harness.KindBuild, Workspace: dir, Prompt: prompt, MaxBudgetUSD: r.Budget(res.Goal), Label: label, MCPServers: mcp, Model: r.Model}

	cost := &costReporter{}
	var mu sync.Mutex
	lastText := "session started"
	sessionID := ""
	report := func(note string) {
		mu.Lock()
		n, sid := note, sessionID
		if n == "" {
			n = lastText
		}
		mu.Unlock()
		if err := r.GW.ReportWorkItemProgress(ctx, item.ID, truncate(n, 2000), cost.delta(), sid); err != nil {
			r.logf("item %s: progress: %v", label, err)
		}
	}
	_ = r.record(ctx, dir, res, "session started")
	report("session started: " + spec.Branch)

	every := r.ProgressEvery
	if every <= 0 {
		every = 60 * time.Second
	}
	tickCtx, stopTicker := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-tickCtx.Done():
				return
			case <-t.C:
				report("")
			}
		}
	}()
	result, runErr := r.Harness.Run(ctx, job, func(p harness.Progress) {
		mu.Lock()
		if p.Text != "" {
			lastText = p.Text
		}
		if p.SessionID != "" {
			sessionID = p.SessionID
		}
		mu.Unlock()
		if p.CostUSD > 0 {
			cost.observe(p.CostUSD)
		}
	})
	stopTicker()
	if ctx.Err() != nil {
		r.logf("item %s: node shutting down; left for a retry", label)
		return ctx.Err()
	}
	cost.observe(result.CostUSD)
	if result.SessionID != "" {
		mu.Lock()
		sessionID = result.SessionID
		mu.Unlock()
	}
	if runErr != nil {
		report("harness error: " + runErr.Error())
		return r.fail(ctx, item, runErr)
	}
	if !result.Success {
		report("session ended without success: " + result.Text)
		return r.fail(ctx, item, fmt.Errorf("session ended without success: %s", truncate(result.Text, 500)))
	}
	report(result.Text)
	_ = r.record(ctx, dir, res, result.Text)

	sha, err := r.WS.Push(ctx, dir, spec.Branch)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("push: %w", err))
	}
	if err := r.GW.FinishWorkItem(ctx, item.ID, spec.Branch, sha, cost.delta()); err != nil {
		return fmt.Errorf("finish %s: %w", label, err)
	}
	r.logf("item %s: finished %s@%s ($%.2f)", label, spec.Branch, short(sha), result.CostUSD)
	return nil
}

// record writes the goal document into the workspace and commits it on
// the item's branch (mesh_record's work, in poll mode).
func (r *Runner) record(ctx context.Context, dir string, res *Resolved, summary string) error {
	items, err := r.GW.GoalWorkItems(ctx, res.Goal.ID)
	if err != nil {
		return err
	}
	goal, err := r.GW.Goal(ctx, res.Goal.ID)
	if err != nil {
		goal = res.Goal
	}
	parent := ""
	if cx, err := FetchContext(ctx, r.GW, r.Scopes.LoginOf(res.Item.OrgID), res.Project.Slug); err == nil {
		parent = cx.PRDSlug
	}
	return RecordGoal(ctx, r.WS, dir, goal, res.Item.Seq, parent, items, summary)
}

// RecordGoal writes specs/goal-<slug>.md and commits it on the current
// branch: `docs(mesh): goal <slug> — item <seq>`.
func RecordGoal(ctx context.Context, ws Workspacer, dir string, goal *gateway.Goal, seq int, parent string, items []gateway.WorkItem, summary string) error {
	doc := GoalDoc{Slug: goal.Slug, Title: goal.Title, Statement: goal.Statement, Acceptance: goal.Acceptance, GoalStatus: goal.Status, Parent: parent, Items: items}
	rel, changed, err := WriteGoalDoc(dir, doc, LogLine(time.Now(), seq, summary))
	if err != nil || !changed {
		return err
	}
	return ws.Commit(ctx, dir, []string{rel}, fmt.Sprintf("docs(mesh): goal %s — item %d", goal.Slug, seq))
}

func (r *Runner) fail(ctx context.Context, item gateway.WorkItem, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.logf("item %s: failed: %v", item.ID, cause)
	if err := r.GW.FailWorkItem(ctx, item.ID, truncate(cause.Error(), 2000)); err != nil && !errors.Is(err, context.Canceled) {
		r.logf("item %s: fail: %v", item.ID, err)
	}
	return cause
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// UserEnv is the environment a session inherits (the operator's).
func UserEnv() []string { return os.Environ() }
