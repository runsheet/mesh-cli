package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
)

// Planner is the session that compiles a goal into its graph (mesh-specs
// prd, the planner; trd §5 phase 2): the project's repos checked out
// read-only, its context from runsheet, the statement and the acceptance; a
// fresh `claude -p` that reads and then writes the items as JSON; the
// graph handed in with PlanGoal, the session's cost with it. A goal
// whose plan cannot be read three times is left in planning and said so.
type Planner struct {
	GW Gateway
	// noGuidance logs once per kind whose brief carries no guidance
	// (ADR-041).
	noGuidance NoGuidance
	Harness    harness.Harness
	WS         Workspacer
	Cfg        *Config
	// Layout, when set, is the node's job and connection trees (ADR-039):
	// the session works in its goal's or direction's job. Nil keeps
	// WorkRoot (the tests' layout).
	Layout   *Layout
	WorkRoot string
	// BuildCache is a host session's GOCACHE; "" is BuildCacheDir().
	BuildCache string
	// Sandbox is the node's sandbox, when its sessions run in pods: a
	// host session (nil) gets a scratch of its own (sessionScratch).
	Sandbox *sandbox.Sandbox
	// Served is the orgs the node serves now (the loop's CurrentScopes):
	// with no Sandbox and two or more of them, no session runs on the
	// host (ADR 066, decision 10; openScopePod). Nil serves one.
	Served func() Scopes
	// MCP is the built-in MCP server the session gets (runsheet);
	// the tools attached for the planner role are rendered beside it
	// (SelectTools, trd §3 Tools).
	MCP map[string]any
	// Proxies, when set, start each session's sessionproxy: every MCP
	// server reached through one socket, no credential in the session
	// (SessionTools.UseVia).
	Proxies *SessionProxies
	// Runtimes are the tool runtimes the node has now (Loop.ToolRuntimes;
	// nil: none), which a local tool needs to start.
	Runtimes func() []string
	// Declines remembers a goal not planned for a required tool whose
	// runtime the node lacks, so the loop does not claim its plan again
	// while the runtimes are unchanged (nil: not remembered).
	Declines *Declines
	// RuntimeBroken is told the error when a session's pod could not
	// start on a broken container runtime (a *sandbox.RuntimeError) and
	// its work was handed back runtime_broken; nil tells no one.
	RuntimeBroken func(error)
	// MaxBudgetUSD caps the planning session (5 by default).
	MaxBudgetUSD float64
	// Budgets is node.yaml's `budgets:` in force, read live (ADR 012,
	// Loop.KindModelBudget); nil falls back to Cfg's own, read once at
	// start.
	Budgets func(kind, model string) (amount float64, source string)
	// ContextBudget is node.yaml's `context:` budget in force, read
	// live (Loop.ContextBudget); nil is Cfg's own, read once at start.
	ContextBudget func() ContextConfig
	// Model is what a planner session runs on ("" the CLI's default).
	Model string
	// Credentials asks the platform for the session's credentials and
	// reports its usage record. Required: a nil one is a programming
	// error, and its Open panics (no node.yaml fallback, adr-005).
	Credentials *Credentials
	// Timeout caps its wall clock (0 none).
	Timeout time.Duration
	Log     *log.Logger

	mu       sync.Mutex
	attempts map[string]int
	// fallback says once per run that the platform issues no plan
	// tokens (adr-022): the planner's own, apart from the items'.
	fallback TokenFallback
	// noRelease says once per run that the platform takes no
	// releasePlan (adr-022, A fetch that fails is reported).
	noRelease sync.Once
	// noGoalRelease says once that the platform takes no releaseGoalPlan.
	noGoalRelease sync.Once
}

// PlanFetchError is a plan's failure before its session opens: a repo's
// plan token refused or missing, its checkout's authorization, or its
// fetch (adr-022). Repo is the owner/name it names, "" when the
// platform's refusal named none we could pick out; Err is the whole
// error, its text the release's note.
type PlanFetchError struct {
	Repo string
	Err  error
}

func (e *PlanFetchError) Error() string { return e.Err.Error() }
func (e *PlanFetchError) Unwrap() error { return e.Err }

// ToolsDeclinedError is a plan declined before its session opens: a
// tool attached for the planner role is required and needs a runtime
// this node lacks (trd §3 Tools). Reason is SessionTools.DeclineReason.
type ToolsDeclinedError struct {
	Reason string
}

func (e *ToolsDeclinedError) Error() string { return "declined, " + e.Reason }

// ToolsDeclined reports whether err is a session declined for its tools.
func ToolsDeclined(err error) bool {
	var td *ToolsDeclinedError
	return errors.As(err, &td)
}

// MaxPlanAttempts is how many unreadable plans a goal gets before the
// planner stops trying (until the node restarts).
const MaxPlanAttempts = 3

func (p *Planner) runtimes() []string {
	if p.Runtimes == nil {
		return nil
	}
	return p.Runtimes()
}

func (p *Planner) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log.Printf(format, args...)
	}
}

// kindModelBudget is node.yaml's budgets: figure for kind and model —
// live, through p.Budgets, when set; else Cfg's own, read once at start.
func (p *Planner) kindModelBudget(kind, model string) (float64, string) {
	if p.Budgets != nil {
		return p.Budgets(kind, model)
	}
	return p.Cfg.KindModelBudget(kind, model)
}

// budgetWithSource is the plan's cost cap (ADR 012) and where it came
// from, for the session's starting log line.
func (p *Planner) budgetWithSource(goal *gateway.Goal) (float64, string) {
	kindBudget, kindSource := p.kindModelBudget("plan", p.Model)
	fallback := p.MaxBudgetUSD
	if fallback <= 0 {
		fallback = 5
	}
	return SessionBudget(kindBudget, kindSource, goal, fallback)
}

// PlanDir is where a goal's planner works: <root>/<goal-slug>/plan.
func PlanDir(root, goalSlug string) string { return filepath.Join(root, goalSlug, "plan") }

// Plan compiles one goal in planning and hands the graph in. An error
// means nothing was handed in.
func (p *Planner) Plan(ctx context.Context, goal gateway.Goal, scope Scope) error {
	// every command the plan sends acts as its planner (ADR-015)
	ctx = actorContext(ctx, p.Cfg, gateway.RolePlanner)
	// the node a finish is handed back by (finishJob, runsheet ADR 061)
	ctx = gateway.WithNodeID(ctx, scope.NodeID)
	ctx = gateway.WithJobKind(ctx, "runsheet/plan")
	p.mu.Lock()
	if p.attempts == nil {
		p.attempts = map[string]int{}
	}
	tries := p.attempts[goal.ID]
	p.mu.Unlock()
	if tries >= MaxPlanAttempts {
		return nil // said so already
	}
	// a plan the product refuses is the product's to count (runsheet
	// counts a plan job that ends without a plan on the goal): the node
	// keeps no per-goal record of refusals
	err := p.plan(ctx, goal, scope)
	if _, ok := NoCredentialOf(err); ok {
		// no credential issued is a hold, not a try: the claim lapses,
		// and the loop asks again once Loop.NoCredential's re-check is due
		return err
	}
	if _, ok := NoHeadroomOf(err); ok {
		// issued credentials with no headroom are a hold too, until the
		// earliest resets: the claim lapses and no try is counted
		return err
	}
	var td *ToolsDeclinedError
	if errors.As(err, &td) {
		// a required tool this node cannot start is a hold, not a try:
		// the claim goes back for a node that can (or lapses on a
		// platform without releasePlan), and Declines keeps the loop off
		// the goal while the runtimes are unchanged
		switch rerr := p.GW.ReleasePlan(ctx, goal.ID, scope.NodeID, td.Reason); {
		case rerr == nil:
			p.logf("goal %s: %v; plan claim released", goal.Slug, err)
		case gateway.IsUnsupported(rerr):
			p.logf("goal %s: %v; the claim lapses", goal.Slug, err)
		default:
			_ = handInRefused(p.logf, nil, goal.Slug, "ReleasePlan", rerr)
			p.logf("goal %s: %v; plan release refused: %v", goal.Slug, err, rerr)
		}
		return err
	}
	if re := RuntimeErrorOf(err); re != nil && ctx.Err() == nil {
		// the session's pod could not start on a broken container
		// runtime: the node's condition, not the goal's — the claim goes
		// back with runtime_broken and no try is counted (ADR-030)
		p.releaseRuntime(ctx, goal, scope, re)
		return err
	}
	if pe, ok := PodStartErrorOf(err); ok && ctx.Err() == nil {
		// the goal's org's pod did not start: the node's condition, not
		// the goal's — the claim goes back with the reason, no try is
		// counted, and the goal is held off this node a while (ADR-034)
		p.releasePodFailure(ctx, goal, scope, pe.Error())
		return err
	}
	if errors.Is(err, ErrHostServesSeveralOrgs) && ctx.Err() == nil {
		// no sandbox on a node serving several orgs: never on the host
		noteHostFallback(err, p.logf)
		p.releasePodFailure(ctx, goal, scope, HostFallbackReason(err))
		return err
	}
	var fe *PlanFetchError
	if errors.As(err, &fe) && ctx.Err() == nil {
		// a fetch or a plan token that failed before the session opened
		// is reported: the claim is released with the error, the goal's
		// planError on the platform, which holds the record from here —
		// no try is counted (adr-022, A fetch that fails is reported)
		rerr := p.GW.ReleasePlan(ctx, goal.ID, scope.NodeID, err.Error())
		switch {
		case rerr == nil:
			p.logf("goal %s: fetch failed: %v; plan claim released", goal.Slug, err)
			return err
		case gateway.IsUnsupported(rerr):
			p.noRelease.Do(func() {
				p.logf("goal %s: the platform takes no plan release; the claim lapses and the try counts (said once per run)", goal.Slug)
			})
		default:
			_ = handInRefused(p.logf, nil, goal.Slug, "ReleasePlan", rerr)
			p.logf("goal %s: fetch failed: %v; plan release refused: %v", goal.Slug, err, rerr)
		}
	}
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

// projectRepoMap reads the goal's project and its enrolled repos, by
// owner/name: the project's repoIds, each owned by its own account's
// login — a repo under another account the org links is the project's
// as much as one under the org's own.
func (p *Planner) projectRepoMap(ctx context.Context, goal gateway.Goal, scope Scope) (*gateway.Project, map[string]gateway.Repo, error) {
	project, err := p.GW.Project(ctx, goal.ProjectID)
	if err != nil {
		return nil, nil, fmt.Errorf("project: %w", err)
	}
	if len(project.RepoIDs) == 0 {
		return nil, nil, fmt.Errorf("project %s has no repos enrolled; nothing to plan in", project.Slug)
	}
	all, err := p.GW.ProjectRepos(ctx, project.RepoIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("repos: %w", err)
	}
	repos := map[string]gateway.Repo{} // owner/name → repo
	for _, r := range all {
		repos[r.FullName()] = r
	}
	return project, repos, nil
}

// goalRepo is the goal's own repo for the repo rule (ADR-023 (d),
// planRepoRule, item 6): Goal.RepoID among the project's repos when the
// platform names one (GoalIndex.repoId — always "" today, since the
// field is not yet in fields/GoalIndex.txt: ADR-010, a field the
// platform has not shipped cannot be selected), else the project's one
// repo when it has exactly one. ok is false when neither resolves one —
// several repos and no named one — and the rule does not apply (the
// project's own check, that an item names one of the project's repos,
// stands either way).
func goalRepo(goal gateway.Goal, project *gateway.Project, repos map[string]gateway.Repo) (gateway.Repo, bool) {
	if id := strings.TrimSpace(goal.RepoID); id != "" {
		for _, r := range repos {
			if r.ID == id {
				return r, true
			}
		}
	}
	if len(project.RepoIDs) == 1 {
		for _, r := range repos {
			if r.ID == project.RepoIDs[0] {
				return r, true
			}
		}
	}
	return gateway.Repo{}, false
}

func (p *Planner) plan(ctx context.Context, goal gateway.Goal, scope Scope) error {
	project, repos, err := p.projectRepoMap(ctx, goal, scope)
	if err != nil {
		return err
	}
	dir, err := planDirFor(p.Layout, scope, goal, p.WorkRoot)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(repos))
	for name := range repos {
		names = append(names, name)
	}
	sort.Strings(names)
	// the goal's own GitHub tokens, one per repo, before anything
	// fetches: each checkout reaches its repo on its own row through its
	// own helper, never the operator's gh (adr-022); nil on an old
	// platform, which fetches on the operator's gh as before
	toks, err := p.planTokens(ctx, goal, scope.NodeID, names)
	if err != nil {
		return err
	}
	dirs := map[string]string{}
	for _, name := range names {
		sub := filepath.Join("repos", repos[name].Name)
		path := filepath.Join(dir, sub)
		if toks != nil {
			if err := p.WS.Authorize(ctx, PlanRepoSpec(name, path), toks[name]); err != nil {
				return &PlanFetchError{Repo: name, Err: fmt.Errorf("authorize %s: %w", name, err)}
			}
		}
		if err := p.WS.Fetch(ctx, name, path); err != nil {
			return &PlanFetchError{Repo: name, Err: fmt.Errorf("fetch %s: %w", name, err)}
		}
		dirs[name] = sub
	}
	// the session's own GH_TOKEN: the first repo's row by sorted name,
	// where exactly one is asked for (adr-022)
	var sessionTok *gateway.GitHubToken
	if toks != nil {
		sessionTok = toks[names[0]]
	}
	cx, err := FetchContext(ctx, p.GW, scope.Name(), project.Slug, ContextOptions{Budget: p.contextBudget(), Repos: names, Checkouts: checkoutsUnder(dir, dirs), Goal: &goal, Logf: p.logf})
	if err != nil {
		p.logf("goal %s: context unavailable: %v", goal.Slug, err)
		cx = &Context{}
	}
	files, written, err := WriteContext(dir, cx)
	if err != nil {
		return fmt.Errorf("write context: %w", err)
	}
	p.logf("goal %s: %s", goal.Slug, written.Line())
	if w := written.TargetWarning(); w != "" {
		p.logf("goal %s: %s", goal.Slug, w)
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 || cx.Live != nil {
		contextDir = ContextDir
	}
	// a re-plan: the items so far, and where the numbering goes on
	var existing []harness.ExistingItem
	var merged []int
	nextSeq := 1
	// the goal's items, kept for the re-land check (ADR-021)
	goalItems, err := p.GW.GoalWorkItems(ctx, goal.ID)
	if err == nil {
		for _, it := range goalItems {
			note := it.Evaluation
			if it.LastError != "" {
				note = it.LastError
			}
			existing = append(existing, harness.ExistingItem{Seq: it.Seq, Title: it.Title, Status: it.Status, Note: truncate(note, 300), Branch: it.Branch, PRURL: it.PRURL, Acceptance: it.Acceptance, LineageRootSeq: it.LineageRootSeq, LineageAttempt: it.LineageAttempt})
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
	replacing := p.replacingItem(goal, goalItems)
	in := harness.PlanInput{
		GoalID: goal.ID, GoalSlug: goal.Slug, GoalTitle: goal.Title, Statement: goal.Statement, Acceptance: goal.Acceptance, BudgetCents: goal.BudgetCents,
		Org: scope.Name(), Project: project.Slug, Repos: dirs, ContextDir: contextDir, ContextFiles: files, Blocked: goal.PausedReason,
		Existing: existing, NextSeq: nextSeq, MeshContext: len(p.MCP) > 0,
		// the project's cap on a plan's items, read at claim time
		// (ADR-023 (a); 8 until the platform serves maxItems)
		MaxItems: project.ItemCap(),
	}
	if replacing != nil {
		// a narrow re-plan (runsheet ADR 023's third rung): the planner is
		// asked to split the failed item, shown its acceptance and every
		// clause its evaluations failed (ADR-021's amendment)
		root, attempt := lineageOf(*replacing)
		in.Replacing = &harness.ExistingItem{Seq: replacing.Seq, Title: replacing.Title, Status: replacing.Status, Acceptance: replacing.Acceptance, LineageRootSeq: root, LineageAttempt: attempt}
		in.ReplacingClauses = p.failedClauses(ctx, goal.Slug, *replacing)
		p.logf("goal %s: narrow re-plan replacing item %d (%s): asking for a split", goal.Slug, replacing.Seq, lineageWords(root, attempt))
	}
	budget, budgetSource := p.budgetWithSource(&goal)
	p.logf("goal %s: planning in %s (%d repos), budget $%.2f (%s)", goal.Slug, dir, len(repos), budget, budgetSource)
	ctx = withSubject(ctx, scope, goal.ProjectID, project.Slug)
	cost, acct, err := p.ask(ctx, goal, scope, dir, budget, in, repos, sessionTok)
	acct.SetContextBytes(written.Bytes)
	acct.SetLabel("goal "+goal.Slug, string(harness.KindPlan))
	defer acct.Finish(ctx, harness.Result{})
	if err != nil {
		return err
	}
	p.logf("goal %s: plan handed in through the product's tool; job closed ($%.2f)", goal.Slug, cost)
	return nil
}

// planTokens reads the goal's plan GitHub tokens (adr-022), keyed by
// each of names (owner/name). A refusal, or a repo with no row, is a
// *PlanFetchError naming it, before any fetch; an old platform
// (gateway.IsUnsupported) is nil, nil, said once per run: the fetch
// runs on the operator's gh as before.
func (p *Planner) planTokens(ctx context.Context, goal gateway.Goal, nodeID string, names []string) (map[string]*gateway.GitHubToken, error) {
	rows, err := p.GW.PlanGitHubTokens(ctx, goal.ID, nodeID)
	if gateway.IsUnsupported(err) {
		p.fallback.NotePlan(goal.Slug, p.logf)
		return nil, nil
	}
	if err != nil {
		// the platform's refusal names the repo it could not reach
		fe := &PlanFetchError{Err: fmt.Errorf("plan github token: %w", err)}
		for _, name := range names {
			if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(name)) {
				fe.Repo = name
				break
			}
		}
		return nil, fe
	}
	byRepo := map[string]*gateway.GitHubToken{}
	for i := range rows {
		byRepo[strings.ToLower(rows[i].Repository)] = &rows[i]
	}
	toks := map[string]*gateway.GitHubToken{}
	for _, name := range names {
		tok := byRepo[strings.ToLower(name)]
		if tok == nil {
			return nil, &PlanFetchError{Repo: name, Err: fmt.Errorf("plan github token: the platform issued none for %s", name)}
		}
		toks[name] = tok
	}
	return toks, nil
}

// replacingItem is the goal's item a narrow re-plan replaces
// (goal.ReplacingItemID among goalItems), nil for a whole plan. An id
// the items do not hold is said and planned as whole: PlanGoal still
// keeps every other item.
func (p *Planner) replacingItem(goal gateway.Goal, goalItems []gateway.WorkItem) *gateway.WorkItem {
	id := strings.TrimSpace(goal.ReplacingItemID)
	if id == "" {
		return nil
	}
	for i := range goalItems {
		if goalItems[i].ID == id {
			return &goalItems[i]
		}
	}
	p.logf("goal %s: narrow re-plan replacing item %s, which is not among the goal's items; planning without the split", goal.Slug, id)
	return nil
}

// failedClauses are every clause the item's recorded evaluations
// failed, oldest attempt first; none when the platform serves no
// records, said.
func (p *Planner) failedClauses(ctx context.Context, goalSlug string, it gateway.WorkItem) []harness.FailedClause {
	was, err := p.GW.WorkItemPriorAttempts(ctx, it.ID)
	if err != nil {
		p.logf("goal %s: item %d's evaluations unavailable: %v", goalSlug, it.Seq, err)
		return nil
	}
	sort.SliceStable(was, func(i, j int) bool { return was[i].Attempt < was[j].Attempt })
	var out []harness.FailedClause
	for _, a := range was {
		for _, c := range a.Clauses {
			out = append(out, harness.FailedClause{Clause: c.Clause, Satisfy: c.Satisfy})
		}
	}
	return out
}

// ask runs one planner session on in. The session reads its goal with
// the kind's brief tool and hands its plan to the kind's finish tool,
// both on the product's own MCP server; the node then closes the job
// with an empty result and reads nothing of the plan (runsheet ADR 077:
// the node knows nothing of what a plan is). It returns the session's
// cost and its account, whose usage report is built as the session
// ends: the caller Finishes it.
func (p *Planner) ask(ctx context.Context, goal gateway.Goal, scope Scope, dir string, budget float64, in harness.PlanInput, repos map[string]gateway.Repo, tok *gateway.GitHubToken) (float64, *SessionAccount, error) {
	nodeID := scope.NodeID
	acct, err := p.Credentials.Open(ctx, gateway.SessionRef{Kind: string(harness.KindPlan), GoalID: goal.ID}, nodeID, Adapter(p.Harness))
	if err != nil {
		// an ErrNoCredential among them: no session starts, and the loop
		// leaves the goal's plan unclaimed (Loop.NoCredential)
		return 0, nil, err
	}
	defer unlessDetached(ctx, acct.Cleanup)
	// the tools attached for the planner role, beside runsheet: a
	// required one this node cannot start starts no session, and the
	// goal's plan is left for a node that can — chosen before the prompt
	// is rendered, so it can name them (ToolHint, the-director-and-the-
	// planner-get-their-tools)
	runtimes := p.runtimes()
	st := SelectTools(p.MCP, acct.Tools, tools.RoleFor(string(harness.KindPlan)), runtimes)
	if len(st.Missing) > 0 {
		p.Declines.Record(goal.ID, st.Missing, runtimes, time.Now())
		return 0, nil, &ToolsDeclinedError{Reason: st.DeclineReason()}
	}
	// Tools: the served tools, so PlanPrompt can name them and say when
	// to search.
	in.Tools = toolHints(st.Use)
	// the plan goes to the product's own tool, which its guidance names:
	// the node hosts no finish tool of its own for it
	brief := jobBriefOf(ctx, p.GW, PlanKind, goal.ID)
	if brief.FinishTool == "" {
		return 0, acct, fmt.Errorf("the %s brief names no finish tool to hand the plan to", PlanKind)
	}
	if brief.Guidance == "" {
		p.noGuidance.Note(PlanKind, p.logf)
	}
	in.Guidance, in.Finish = brief.ProductGuidance(false), brief.HandInSection()
	kept, err := keptRootFor(p.Layout, goal.ID)
	if err != nil {
		return 0, acct, err
	}
	job := harness.Job{Kind: harness.KindPlan, Workspace: dir, Prompt: harness.PlanPrompt(in), MaxBudgetUSD: budget, Label: goal.Slug + " plan", Timeout: p.Timeout, MCPServers: p.MCP, Model: p.Model, KeptRoot: kept}
	// the goal's job's scratch, else the work root's (dir is
	// PlanDir(root, slug): the work root is two up)
	scratchRoot, err := scratchRootFor(p.Layout, goal.ID, filepath.Dir(filepath.Dir(dir)))
	if err != nil {
		return 0, acct, err
	}
	// the goal's org's pod, opened before anything runs: a pod that
	// cannot start runs no session, never on the host in its place
	// (ADR-034, Refusals); nil on an unsandboxed node serving one org
	pod, err := openScopePod(ctx, p.Sandbox, p.Layout, servedOf(p.Served), scope, dir, job.Label, harness.KindPlan, p.logf)
	// a session the harness detached runs on in its pod (Session.Detach
	// makes this Close nothing): the node that adopts it removes it
	defer pod.Close()
	if err != nil {
		return 0, acct, err
	}
	pod.Apply(&job)
	if tok != nil {
		// gh in the session runs on the goal's token and a config dir of
		// its own, never the operator's login (adr-022), under the org's
		// scratch on a sandboxed node
		ghDir, err := sessionGHConfigDir(p.Sandbox, scratchRoot)
		if err != nil {
			return 0, acct, fmt.Errorf("gh config dir: %w", err)
		}
		// a session left running for the next node keeps it: the node
		// that ends it removes it (TempDirs)
		defer unlessDetached(ctx, func() { os.RemoveAll(ghDir) })
		job.Env = SessionEnv(tok, ghDir)
	}
	// the session's own scratch as its TMPDIR, gone with the session
	// (sessionScratch); a detached one's by the node that ends it
	scratch, err := sessionScratch(p.Sandbox, scratchRoot, p.BuildCache, harness.KindPlan, goal.ID, &job)
	if err != nil {
		return 0, acct, err
	}
	defer unlessDetached(ctx, func() { RemoveScratch(scratch) })
	acct.Apply(&job)
	job.BriefServers = briefServers(ctx, p.GW, PlanKind, goal.ID)
	closeProxy, err := st.UseVia(p.Proxies, mcpRootFor(p.Layout, goal.ID), &job, p.MCP, acct)
	if err != nil {
		return 0, acct, fmt.Errorf("tools: %w", err)
	}
	defer closeProxy(ctx)
	if len(st.Use) > 0 {
		p.logf("goal %s: tools %s", goal.Slug, st.Names())
	}
	if note := st.OmittedNote(); note != "" {
		p.logf("goal %s: %s", goal.Slug, note)
	}
	job.Meta = harness.SessionMeta{NodeID: nodeID, GoalID: goal.ID, TempDirs: sessionTempDirs(job, acct)}
	// the session outlives this process: a restart adopts it (ADR 009)
	job.Outlive = true
	result, err := p.Harness.Run(ctx, job, nil)
	acct.Report(result)
	if note := acct.UseNote(result); note != "" && err == nil {
		p.logf("goal %s: %s", goal.Slug, note)
	}
	cost, err := p.closePlan(ctx, goal, scope, dir, result, err)
	return cost, acct, err
}

// closePlan is what follows a planner session, however this process came
// to hold it: a session that failed or ran out of time is the plan's
// error; one that ended is closed with finishJob's empty result, which
// the product refuses when no plan reached its finish tool (and counts).
// A cancelled ctx leaves the session running for the next node to adopt;
// otherwise its session directory goes.
func (p *Planner) closePlan(ctx context.Context, goal gateway.Goal, scope Scope, dir string, result harness.Result, err error) (float64, error) {
	logKilled(p.logf, "goal "+goal.Slug, result.KilledPerSession(), result.KillError)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	defer harness.RemoveSession(dir)
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, fmt.Errorf("planner session ran out of time (%s)", p.Timeout)
	}
	if err != nil {
		return 0, fmt.Errorf("planner session: %w", err)
	}
	if _, ferr := p.GW.FinishJob(ctx, scope.NodeID, PlanKind, gateway.JobSubject{Type: "goal", ID: goal.ID}, map[string]any{"result": map[string]any{}}); ferr != nil {
		return result.CostUSD, fmt.Errorf("planner session (%s) ended; the product refused to close the job: %w", result.SessionID, ferr)
	}
	return result.CostUSD, nil
}

// PlanKind is runsheet's plan kind, the job a plan is.
const PlanKind = "runsheet/plan"

// AdoptPlan takes up a planner session a node process before this one
// started (ADR 009): followed to its end, or read from its file when it
// is gone, its graph read and handed in. A plan the session left flagged
// (adr-004) is handed in as it stands: its prompt is not kept to ask
// again. A goal no longer in planning is not its to plan: the session is
// stopped and its directory removed.
func (p *Planner) AdoptPlan(ctx context.Context, goal gateway.Goal, scope Scope, s FoundSession) error {
	ctx = actorContext(ctx, p.Cfg, gateway.RolePlanner)
	// the node a finish is handed back by (finishJob, runsheet ADR 061)
	ctx = gateway.WithNodeID(ctx, scope.NodeID)
	if goal.Status != "planning" {
		p.logf("goal %s: adopted plan is no longer wanted (%s); stopped", goal.Slug, goal.Status)
		stopFound(p.logf, "goal "+goal.Slug, s)
		return s.Remove()
	}
	at, ok := p.Harness.(harness.Attacher)
	if !ok {
		stopFound(p.logf, "goal "+goal.Slug, s)
		s.Remove()
		return errNoAttach
	}
	endProxy := adoptProxy(ctx, p.Proxies, p.Credentials, p.MCP, tools.RoleFor(string(harness.KindPlan)), p.runtimes(), gateway.SessionRef{Kind: string(harness.KindPlan), GoalID: goal.ID}, s, "goal "+goal.Slug, nil, p.logf)
	defer endProxy(ctx)
	p.logf("goal %s: adopted its %s planner session (pid %d)", goal.Slug, s.Liveness(), s.State.PID)
	result, err := at.Attach(ctx, s.Dir, nil)
	if _, err := p.closePlan(ctx, goal, scope, s.State.Workspace, result, err); err != nil {
		return err
	}
	p.logf("goal %s: adopted plan closed", goal.Slug)
	return nil
}

// redactGraph is v with every string in it redacted.
func redactGraph(v any, redact func(string) string) any {
	switch t := v.(type) {
	case string:
		return redact(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = redactGraph(val, redact)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactGraph(val, redact)
		}
		return out
	}
	return v
}

// contextBudget is the context budget for p's sessions: live, through
// p.ContextBudget, when set; else Cfg's own.
func (p *Planner) contextBudget() ContextConfig {
	if p.ContextBudget != nil {
		return p.ContextBudget()
	}
	if p.Cfg == nil {
		return ContextConfig{}.Effective()
	}
	return p.Cfg.Context.Effective()
}
