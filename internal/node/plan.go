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
	"projectmesh.io/mesh-cli/internal/tools"
)

// Planner is the session that compiles a goal into its graph (mesh-specs
// prd, the planner; trd §5 phase 2): the project's repos checked out
// read-only, its context from mesh-context, the statement and the acceptance; a
// fresh `claude -p` that reads and then writes the items as JSON; the
// graph handed in with PlanGoal, the session's cost with it. A goal
// whose plan cannot be read three times is left in planning and said so.
type Planner struct {
	GW       Gateway
	Harness  harness.Harness
	WS       Workspacer
	Cfg      *Config
	WorkRoot string
	// MCP is the built-in MCP server the session gets (mesh-context);
	// the tools attached for the planner role are rendered beside it
	// (SelectTools, trd §3 Tools).
	MCP map[string]any
	// Runtimes are the tool runtimes the node has now (Loop.ToolRuntimes;
	// nil: none), which a local tool needs to start.
	Runtimes func() []string
	// Declines remembers a goal not planned for a required tool whose
	// runtime the node lacks, so the loop does not claim its plan again
	// while the runtimes are unchanged (nil: not remembered).
	Declines *Declines
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
	// lastRefusal is each goal's last plan-check refusal, normalised, and
	// released the goals whose plan claim a second identical one released
	// (ADR-023, a refused plan is released, not re-planned); kept under
	// the work root too (PlanRefusalsPath), so a fresh Planner and a
	// restarted node read the same.
	lastRefusal map[string]string
	released    map[string]releasedPlan
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
	p.mu.Lock()
	if p.attempts == nil {
		p.attempts = map[string]int{}
	}
	tries := p.attempts[goal.ID]
	p.mu.Unlock()
	// a goal released for a repeated refusal is not planned again until
	// its row changes (ADR-023, a refused plan is released, not re-planned)
	p.forgetChanged(goal)
	if reason, ok := p.Released(goal); ok {
		return &PlanRefused{Goal: goal.Slug, Reason: reason, Released: true}
	}
	if tries >= MaxPlanAttempts {
		return nil // said so already
	}
	err := p.plan(ctx, goal, scope)
	if pr, ok := PlanRefusedOf(err); ok && ctx.Err() == nil {
		// the same refusal twice running releases the claim: no third
		// session, no further try counted
		if rel := p.refused(ctx, goal, scope, pr); rel.Released {
			return rel
		}
	}
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
			p.logf("goal %s: %v; plan release refused: %v", goal.Slug, err, rerr)
		}
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

// evidence reads the project's scorecard (item 5, model choice by
// evidence) and builds its per-repo, per-model rows. A platform without
// the query, or a scorecard whose shape carries none of the keys a row
// needs, yields an empty slice — the plan still runs, unadvised.
func (p *Planner) evidence(ctx context.Context, goalSlug string, scope Scope, project *gateway.Project) []harness.ModelEvidence {
	sc, err := p.GW.Scorecard(ctx, scope.ID, project.ID)
	if err != nil {
		p.logf("goal %s: scorecard unavailable: %v", goalSlug, err)
		return nil
	}
	return evidenceFromScorecard(sc)
}

// evidenceFromScorecard walks a scorecard project entry for per-model
// rows, over the widest of its `windows` (runsheet trd §6a,
// `platform/scorecard.Sums.Into`: each window's `models` list, one row
// per model with an item in it — `model`, `evaluated`, `firstPassRate`
// null when nothing was evaluated, `costPerItemCents`). The platform
// carries no repo breakdown yet, so every row from `models` (or its
// tolerated alias `byModel`) comes back with an empty repo — which
// never matches an item's own repo in `modelAgainstEvidence`, so it is
// advice the check has nothing to say about yet, not a wrong match. A
// scorecard that does carry one — a `byRepo` list, each entry's own
// repo and `models`/`byModel` rows — is read repo by repo, at the
// project level and inside each window, for whenever the platform adds
// it. A row needs a model and a first-pass rate to count: `firstPassRate`
// (or `first_pass_rate`) first, else `firstPass`/`first_pass` over
// `evaluated`/`items` when both are present, else `firstPass`/`first_pass`
// itself as the rate; a missing or null rate skips the row. Items are
// the row's `evaluated` (the rate's own denominator), else `items` or
// `merged`. Cost per item is `costPerItemCents`/`cost_per_item_cents`,
// else `costCents`/`cost_cents` over items. Anything else — no
// scorecard, no windows, no such keys — yields none.
func evidenceFromScorecard(sc map[string]any) []harness.ModelEvidence {
	var out []harness.ModelEvidence
	out = append(out, evidenceRows(sc, "")...)
	out = append(out, evidenceByRepo(sc)...)
	if w := widestWindow(sc); w != nil {
		out = append(out, evidenceRows(w, "")...)
		out = append(out, evidenceByRepo(w)...)
	}
	return out
}

// widestWindow is a scorecard entry's own `windows` row with the
// largest `days` — the platform sends one, its own `models` list, per
// trailing window (7 and 30 days today); the widest has the most
// evidence behind each row.
func widestWindow(sc map[string]any) map[string]any {
	list, _ := sc["windows"].([]any)
	var widest map[string]any
	var widestDays float64 = -1
	for _, e := range list {
		w, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if days := numberFieldOr(w, 0, "days"); days > widestDays {
			widest, widestDays = w, days
		}
	}
	return widest
}

// evidenceByRepo reads a `byRepo` list some future scorecard may carry:
// each entry names a repo and its own `models`/`byModel` rows.
func evidenceByRepo(m map[string]any) []harness.ModelEvidence {
	var out []harness.ModelEvidence
	list, _ := m["byRepo"].([]any)
	for _, e := range list {
		row, ok := e.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, evidenceRows(row, stringField(row, "repo"))...)
	}
	return out
}

// evidenceRows reads the flat per-model list off m — `models`, the
// platform's own key, or `byModel`, the tolerated alias — into
// ModelEvidence rows all carrying the given repo.
func evidenceRows(m map[string]any, repo string) []harness.ModelEvidence {
	var out []harness.ModelEvidence
	for _, key := range []string{"models", "byModel"} {
		list, _ := m[key].([]any)
		for _, e := range list {
			row, ok := e.(map[string]any)
			if !ok {
				continue
			}
			model := stringField(row, "model")
			if model == "" {
				continue
			}
			fp, ok := firstPassRateOf(row)
			if !ok {
				continue
			}
			items := int(numberFieldOr(row, 0, "evaluated", "items", "merged"))
			costPerItem := int(numberFieldOr(row, 0, "costPerItemCents", "cost_per_item_cents"))
			if costPerItem == 0 && items > 0 {
				if cost, ok := numberField(row, "costCents", "cost_cents"); ok {
					costPerItem = int(cost / float64(items))
				}
			}
			r := repo
			if r == "" {
				r = stringField(row, "repo")
			}
			out = append(out, harness.ModelEvidence{Repo: r, Model: model, FirstPass: fp, Items: items, CostPerItemCents: costPerItem})
		}
	}
	return out
}

// firstPassRateOf is a model row's first-pass rate: `firstPassRate` (or
// `first_pass_rate`) when the platform sent one — null reads as "no
// rate", not zero, since nothing was evaluated — else `firstPass`/
// `first_pass` over `evaluated`/`items` when both are counts, else
// `firstPass`/`first_pass` taken as the rate itself.
func firstPassRateOf(row map[string]any) (float64, bool) {
	if v, ok := row["firstPassRate"]; ok {
		n, ok := v.(float64)
		return n, ok
	}
	if v, ok := row["first_pass_rate"]; ok {
		n, ok := v.(float64)
		return n, ok
	}
	fp, hasFP := numberField(row, "firstPass", "first_pass")
	if !hasFP {
		return 0, false
	}
	if items, ok := numberField(row, "evaluated", "items"); ok && items > 0 {
		return fp / items, true
	}
	return fp, true
}

func stringField(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return s
}

func numberField(row map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if n, ok := row[k].(float64); ok {
			return n, true
		}
	}
	return 0, false
}

func numberFieldOr(row map[string]any, def float64, keys ...string) float64 {
	if n, ok := numberField(row, keys...); ok {
		return n
	}
	return def
}

func (p *Planner) plan(ctx context.Context, goal gateway.Goal, scope Scope) error {
	project, repos, err := p.projectRepoMap(ctx, goal, scope)
	if err != nil {
		return err
	}
	dir := PlanDir(p.WorkRoot, goal.Slug)
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
		GoalSlug: goal.Slug, GoalTitle: goal.Title, Statement: goal.Statement, Acceptance: goal.Acceptance, BudgetCents: goal.BudgetCents,
		Org: scope.Name(), Project: project.Slug, Repos: dirs, ContextDir: contextDir, ContextFiles: files, Blocked: goal.PausedReason,
		Existing: existing, NextSeq: nextSeq, MeshContext: len(p.MCP) > 0, Evidence: p.evidence(ctx, goal.Slug, scope, project),
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
	gr, hasGoalRepo := goalRepo(goal, project, repos)
	items, reasons, flags, questions, checkpoints, drafts, cost, acct, err := p.ask(ctx, goal, scope.NodeID, dir, budget, in, repos, merged, goalItems, replacing, sessionTok, gr, hasGoalRepo)
	acct.SetContextBytes(written.Bytes)
	acct.SetLabel("goal "+goal.Slug, string(harness.KindPlan))
	// the session's usage report rides the plan's hand-in; Finish logs it
	// when nothing carried it (its report was built as it ended, in ask)
	defer acct.Finish(ctx, harness.Result{})
	if err != nil {
		return err
	}
	if drafts != nil {
		return p.handInSplit(ctx, goal, drafts, 0, in.MaxItems, cost, acct)
	}
	if len(flags) > 0 {
		// adr-004: an acceptance no evaluator can see is the plan's error,
		// ADR-020: so is a reading of the host as a merge gate, and
		// ADR-021: so is a failed item recreated at its size; the planner
		// is asked once more with what was flagged
		p.logf("goal %s: plan names what no evaluator can see; asking again: %s", goal.Slug, strings.Join(flags, " | "))
		in.Rejected = flags
		// ADR-023 (a): a graph over the cap is asked for `split` instead
		over := len(items)
		in.OverCap = over > in.MaxItems
		var again float64
		var second *SessionAccount
		items, reasons, flags, questions, checkpoints, drafts, again, second, err = p.ask(ctx, goal, scope.NodeID, dir, budget, in, repos, merged, goalItems, replacing, sessionTok, gr, hasGoalRepo)
		defer second.Finish(ctx, harness.Result{})
		second.SetContextBytes(written.Bytes)
		second.SetLabel("goal "+goal.Slug, string(harness.KindPlan))
		// one hand-in, one report: the first session's rides the second's
		second.Carries(acct)
		if second != nil {
			acct = second
		}
		cost += again
		if err != nil {
			return err
		}
		if drafts != nil {
			if !in.OverCap {
				over = 0
			}
			return p.handInSplit(ctx, goal, drafts, over, in.MaxItems, cost, acct)
		}
		if len(flags) > 0 {
			// adr-004: the second plan is handed in as it stands; the
			// evaluator's unverifiable-clause rule is the backstop (a
			// re-land, or a narrow plan that does not split, never gets
			// here: readPlan refused it)
			p.logf("goal %s: plan still names what no evaluator can see; handing it in: %s", goal.Slug, strings.Join(flags, " | "))
		}
	}
	// a narrow plan hands in its replacement items alone: PlanGoal, while
	// the goal carries replacingItemId, supersedes only that item, keeps
	// every other and rewires the replaced item's dependents onto these
	// (runsheet ADR 023), so the node neither cancels nor recreates them
	return p.handIn(ctx, goal, items, reasons, questions, checkpoints, cost, acct)
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

// handIn hands the graph in with PlanGoal, carrying the planner
// session's usage report (acct's, built as it ended; none for an adopted
// one), and logs each item's model.
func (p *Planner) handIn(ctx context.Context, goal gateway.Goal, items []gateway.PlannedItem, reasons map[int]string, questions []harness.PlanQuestion, checkpoints []string, cost float64, acct *SessionAccount) error {
	by := ""
	if p.Cfg != nil {
		by = p.Cfg.OwnerID
	}
	// ADR-020: the planner's questions for the person ride the plan on a
	// platform that takes them; else each is said once here, and the
	// person reads them in node.log until the platform half lands
	var sent []gateway.PlanQuestion
	if len(questions) > 0 && p.GW.TakesPlanQuestions(ctx) {
		for _, q := range questions {
			sent = append(sent, gateway.PlanQuestion{Clause: q.Clause, Question: q.Question})
		}
	} else {
		for _, q := range questions {
			p.logf("plan %s: question for the person: %s — %s", goal.Slug, q.Clause, q.Question)
		}
	}
	// ADR-023's amendment, the cut-over rule: a `pr` checkpoint beside the
	// items is set on the goal before the plan lands, so a person approves
	// each pull request; any other value is said and dropped
	if p.wantsPRCheckpoint(goal, checkpoints) {
		cps := append(append([]string{}, goal.Checkpoints...), "pr")
		if err := p.GW.SetGoalCheckpoints(ctx, goal.ID, by, cps); err != nil {
			// the graph still lands: a refused checkpoint is said, not
			// the plan's failure
			p.logf("goal %s: pr checkpoint refused: %v", goal.Slug, err)
		} else {
			p.logf("goal %s: pr checkpoint set: a person approves each pull request", goal.Slug)
		}
	}
	if err := acct.Carried(p.GW.PlanGoal(ctx, goal.ID, by, items, Cents(cost), acct.Report(harness.Result{}), sent...)); err != nil {
		return fmt.Errorf("hand in the plan: %w", err)
	}
	switch len(questions) {
	case 0:
		p.logf("goal %s: planned, %d items ($%.2f)", goal.Slug, len(items), cost)
	case 1:
		p.logf("goal %s: planned, %d items and a question for the person ($%.2f)", goal.Slug, len(items), cost)
	default:
		p.logf("goal %s: planned, %d items and %d questions for the person ($%.2f)", goal.Slug, len(items), len(questions), cost)
	}
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

// wantsPRCheckpoint says the plan's checkpoints name `pr` and the goal
// lacks it, logging each value other than `pr` as dropped.
func (p *Planner) wantsPRCheckpoint(goal gateway.Goal, checkpoints []string) bool {
	want := false
	for _, c := range checkpoints {
		if !strings.EqualFold(strings.TrimSpace(c), "pr") {
			p.logf("goal %s: plan checkpoint %q is not one the node sets (pr); dropped", goal.Slug, c)
			continue
		}
		want = true
	}
	if !want {
		return false
	}
	for _, c := range goal.Checkpoints {
		if strings.EqualFold(c, "pr") {
			return false
		}
	}
	return true
}

// handInSplit hands the planner's `split` answer in with
// ProposeGoalSplit, carrying the session's usage report as handIn does
// (ADR-023 (a)): n is how many items the graph over the cap had (0 when
// no graph came first, as for a split answered at once or adopted). A
// platform without proposeGoalSplit is told nothing: each draft is
// logged and the error leaves the goal in planning and says so, as an
// unreadable plan does.
func (p *Planner) handInSplit(ctx context.Context, goal gateway.Goal, drafts []gateway.GoalDraft, n, cap int, cost float64, acct *SessionAccount) error {
	by := ""
	if p.Cfg != nil {
		by = p.Cfg.OwnerID
	}
	slugs := make([]string, 0, len(drafts))
	for _, d := range drafts {
		slugs = append(slugs, d.Slug)
	}
	over := fmt.Sprintf("over the cap of %d", cap)
	if n > 0 {
		over = fmt.Sprintf("over the cap (%d items > %d)", n, cap)
	}
	if !p.GW.TakesGoalSplit(ctx) {
		for _, d := range drafts {
			p.logf("plan %s: proposed goal %s: %s", goal.Slug, d.Slug, d.Title)
		}
		if n > 0 {
			return fmt.Errorf("plan has %d items, over the cap of %d; split into %d goals proposed, which this platform cannot take", n, cap, len(drafts))
		}
		return fmt.Errorf("plan answered over the cap of %d; split into %d goals proposed, which this platform cannot take", cap, len(drafts))
	}
	if err := acct.Carried(p.GW.ProposeGoalSplit(ctx, goal.ID, by, drafts, Cents(cost), acct.Report(harness.Result{}))); err != nil {
		return fmt.Errorf("propose the split: %w", err)
	}
	p.logf("goal %s: %s; proposed a split into %d goals: %s", goal.Slug, over, len(drafts), strings.Join(slugs, ", "))
	return nil
}

// ask runs one planner session on in and reads its graph: the items
// mapped onto the project's repos, each item's model_reason by seq, the
// check's flags on their acceptance (unverifiableAcceptance,
// hostReadingAcceptance, closeOutAcceptance, cutOverAcceptance,
// sameAcceptanceAsFailed
// against goalItems, overCap against in.MaxItems), its questions for
// the person and checkpoints for the goal — or, for a `split` answer,
// its goal drafts in place of all of those (ADR-023 (a)) — the session's cost and its account, whose usage report is built as the
// session ends: the caller hands it in and Finishes it. An error means
// there is no graph to hand in.
func (p *Planner) ask(ctx context.Context, goal gateway.Goal, nodeID, dir string, budget float64, in harness.PlanInput, repos map[string]gateway.Repo, merged []int, goalItems []gateway.WorkItem, replacing *gateway.WorkItem, tok *gateway.GitHubToken, gr gateway.Repo, hasGoalRepo bool) ([]gateway.PlannedItem, map[int]string, []string, []harness.PlanQuestion, []string, []gateway.GoalDraft, float64, *SessionAccount, error) {
	acct, err := p.Credentials.Open(ctx, gateway.SessionRef{Kind: string(harness.KindPlan), GoalID: goal.ID}, nodeID, Adapter(p.Harness))
	if err != nil {
		// an ErrNoCredential among them: no session starts, and the loop
		// leaves the goal's plan unclaimed (Loop.NoCredential)
		return nil, nil, nil, nil, nil, nil, 0, nil, err
	}
	defer unlessDetached(ctx, acct.Cleanup)
	// the tools attached for the planner role, beside mesh-context: a
	// required one this node cannot start starts no session, and the
	// goal's plan is left for a node that can — chosen before the prompt
	// is rendered, so it can name them (ToolHint, the-director-and-the-
	// planner-get-their-tools)
	runtimes := p.runtimes()
	st := SelectTools(p.MCP, acct.Tools, tools.RoleFor(string(harness.KindPlan)), runtimes)
	if len(st.Missing) > 0 {
		p.Declines.Record(goal.ID, st.Missing, runtimes, time.Now())
		return nil, nil, nil, nil, nil, nil, 0, nil, &ToolsDeclinedError{Reason: st.DeclineReason()}
	}
	// Tools: the served tools, so PlanPrompt can name them and say when
	// to search.
	in.Tools = toolHints(st.Use)
	job := harness.Job{Kind: harness.KindPlan, Workspace: dir, Prompt: harness.PlanPrompt(in), MaxBudgetUSD: budget, Label: goal.Slug + " plan", Timeout: p.Timeout, MCPServers: p.MCP, Model: p.Model}
	if tok != nil {
		// gh in the session runs on the goal's token and a config dir of
		// its own, never the operator's login (adr-022)
		ghDir, err := NewGHConfigDir()
		if err != nil {
			return nil, nil, nil, nil, nil, nil, 0, acct, fmt.Errorf("gh config dir: %w", err)
		}
		// a session left running for the next node keeps it: the node
		// that ends it removes it (TempDirs)
		defer unlessDetached(ctx, func() { os.RemoveAll(ghDir) })
		job.Env = SessionEnv(tok, ghDir)
	}
	acct.Apply(&job)
	if err := st.UseTools(&job, p.MCP, acct); err != nil {
		return nil, nil, nil, nil, nil, nil, 0, acct, fmt.Errorf("tools: %w", err)
	}
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
	items, reasons, flags, questions, checkpoints, drafts, cost, err := p.readPlan(ctx, goal, dir, acct, result, err, repos, merged, in.Evidence, goalItems, replacing, len(in.Rejected) > 0, gr, hasGoalRepo, in.MaxItems)
	return items, reasons, flags, questions, checkpoints, drafts, cost, acct, err
}

// readPlan is what follows a planner session, however this process came
// to hold it: its graph read and mapped onto the project's repos, as ask
// describes. A cancelled ctx leaves the session running for the next
// node to adopt; otherwise its session directory goes. A last answer
// (final: the re-ask's, or an adopted session's, which is not asked
// again) that still recreates a failed item at its acceptance, or, on a
// narrow re-plan (replacing set), still does not split the replaced
// item, or still names a repo other than the goal's own (hasGoalRepo,
// ADR-023 (d)), is not handed in: it is an error naming the lineage or
// the repo (ADR-021's amendments, ADR-023).
func (p *Planner) readPlan(ctx context.Context, goal gateway.Goal, dir string, acct *SessionAccount, result harness.Result, err error, repos map[string]gateway.Repo, merged []int, evidence []harness.ModelEvidence, goalItems []gateway.WorkItem, replacing *gateway.WorkItem, final bool, gr gateway.Repo, hasGoalRepo bool, cap int) ([]gateway.PlannedItem, map[int]string, []string, []harness.PlanQuestion, []string, []gateway.GoalDraft, float64, error) {
	// logged before a done ctx returns: a deadline kills the unit too,
	// and a detach kills nothing
	logKilled(p.logf, "goal "+goal.Slug, result.KilledPerSession(), result.KillError)
	if cap <= 0 {
		cap = gateway.DefaultMaxItems
	}
	if ctx.Err() != nil {
		return nil, nil, nil, nil, nil, nil, 0, ctx.Err()
	}
	defer harness.RemoveSession(dir)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("planner session ran out of time (%s)", p.Timeout)
	}
	if err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("planner session: %w", acct.RedactErr(err))
	}
	result.Text = acct.Redact(result.Text)
	// a `split` answer — goal drafts in place of a graph over the
	// project's cap (ADR-023 (a)) — is read first: it has no items
	if drafts, ok := harness.ParsePlanSplit(result.Text); ok {
		gd, err := splitDrafts(drafts, repos, cap)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, 0, err
		}
		return nil, nil, nil, nil, nil, gd, result.CostUSD, nil
	}
	specs, err := harness.ParsePlan(result.Text, merged...)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("planner session (%s): %w: %s", result.SessionID, err, truncate(result.Text, 300))
	}
	items := make([]gateway.PlannedItem, 0, len(specs))
	itemRepo := map[int]gateway.Repo{}
	reasons := map[int]string{}
	for _, sp := range specs {
		r, ok := projectRepo(repos, sp.Repo)
		if !ok {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("item %d names repo %q, not one of the project's", sp.Seq, sp.Repo)
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
		items = append(items, gateway.PlannedItem{Seq: sp.Seq, Title: strings.TrimSpace(sp.Title), Brief: gateway.BriefWithAtomic(gateway.BriefWithPrecondition(briefWithDeps(sp.Brief, deps, because), sp.Precondition), sp.Atomic), Acceptance: strings.TrimSpace(sp.Acceptance), DependsOn: deps, DependsBecause: because, RepoID: r.ID, BaseRef: base, Model: model, Precondition: strings.Join(strings.Fields(sp.Precondition), " ")})
	}
	flags := unverifiableAcceptance(specs, itemRepo, repos)
	flags = append(flags, hostReadingAcceptance(specs)...)
	flags = append(flags, closeOutAcceptance(specs)...)
	flags = append(flags, cutOverAcceptance(specs)...)
	flags = append(flags, modelAgainstEvidence(specs, evidence)...)
	relanded := reLands(specs, goalItems)
	unsplit := ""
	if replacing != nil {
		unsplit = notASplit(specs, *replacing)
	}
	otherRepoRefused := ""
	if hasGoalRepo {
		otherRepoRefused = otherRepoRefusal(specs, itemRepo, gr)
	}
	over := overCap(specs, cap)
	if final && (len(relanded) > 0 || unsplit != "" || otherRepoRefused != "" || over != "") {
		why := make([]string, 0, len(relanded)+3)
		if over != "" {
			why = append(why, overCapRefusal(len(specs), cap))
		}
		for _, r := range relanded {
			why = append(why, r.refusal())
		}
		if unsplit != "" {
			why = append(why, splitRefusal(*replacing, len(specs)))
		}
		if otherRepoRefused != "" {
			why = append(why, otherRepoRefused)
		}
		return nil, nil, nil, nil, nil, nil, 0, &PlanRefused{Goal: goal.Slug, Reason: strings.Join(why, "; ")}
	}
	if over != "" {
		flags = append(flags, over)
	}
	flags = append(flags, sameAcceptanceAsFailed(specs, goalItems)...)
	if unsplit != "" {
		flags = append(flags, unsplit)
	}
	if hasGoalRepo {
		flags = append(flags, otherRepoItems(specs, itemRepo, gr)...)
	}
	return items, reasons, flags, harness.ParsePlanQuestions(result.Text), harness.ParsePlanCheckpoints(result.Text), nil, result.CostUSD, nil
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
	ctx = actorContext(ctx, p.Cfg, gateway.RolePlanner)
	if goal.Status != "planning" {
		p.logf("goal %s: adopted plan is no longer wanted (%s); stopped", goal.Slug, goal.Status)
		stopFound(p.logf, "goal "+goal.Slug, s)
		return harness.RemoveSession(s.State.Workspace)
	}
	project, repos, err := p.projectRepoMap(ctx, goal, scope)
	if err != nil {
		return err
	}
	var merged []int
	goalItems, err := p.GW.GoalWorkItems(ctx, goal.ID)
	if err == nil {
		for _, it := range goalItems {
			if it.Status == "merged" {
				merged = append(merged, it.Seq)
			}
		}
	}
	replacing := p.replacingItem(goal, goalItems)
	at, ok := p.Harness.(harness.Attacher)
	if !ok {
		stopFound(p.logf, "goal "+goal.Slug, s)
		harness.RemoveSession(s.State.Workspace)
		return errNoAttach
	}
	p.logf("goal %s: adopted its %s planner session (pid %d)", goal.Slug, s.Liveness(), s.State.PID)
	result, err := at.Attach(ctx, s.Dir, nil)
	gr, hasGoalRepo := goalRepo(goal, project, repos)
	items, reasons, flags, questions, checkpoints, drafts, cost, err := p.readPlan(ctx, goal, s.State.Workspace, nil, result, err, repos, merged, p.evidence(ctx, goal.Slug, scope, project), goalItems, replacing, true, gr, hasGoalRepo, project.ItemCap())
	if err != nil {
		return err
	}
	if drafts != nil {
		return p.handInSplit(ctx, goal, drafts, 0, project.ItemCap(), cost, nil)
	}
	if len(flags) > 0 {
		p.logf("goal %s: adopted plan names what no evaluator can see; handing it in: %s", goal.Slug, strings.Join(flags, " | "))
	}
	return p.handIn(ctx, goal, items, reasons, questions, checkpoints, cost, nil)
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
