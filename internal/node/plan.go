package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Planner is the session that compiles a goal into its graph (mesh-specs
// prd, the planner; trd §5 phase 2): the project's repos checked out
// read-only, its mesh-context context, the statement and the acceptance; a
// fresh `claude -p` that reads and then writes the items as JSON; the
// graph handed in with PlanGoal, the session's cost with it. A goal
// whose plan cannot be read three times is left in planning and said so.
type Planner struct {
	GW       Gateway
	Harness  harness.Harness
	WS       Workspacer
	Cfg      *Config
	WorkRoot string
	// MCP are the MCP servers the session gets (mesh-context).
	MCP map[string]any
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
	dirs := map[string]string{}
	for name, r := range repos {
		sub := filepath.Join("repos", r.Name)
		if err := p.WS.Fetch(ctx, name, filepath.Join(dir, sub)); err != nil {
			return fmt.Errorf("fetch %s: %w", name, err)
		}
		dirs[name] = sub
	}
	names := make([]string, 0, len(repos))
	for name := range repos {
		names = append(names, name)
	}
	sort.Strings(names)
	cx, err := FetchContext(ctx, p.GW, scope.Name(), project.Slug, ContextOptions{Budget: p.contextBudget(), Repos: names, Logf: p.logf})
	if err != nil {
		p.logf("goal %s: context unavailable: %v", goal.Slug, err)
		cx = &Context{}
	}
	AddLive(ctx, p.GW, cx, scope.ID, project.ID, p.logf)
	files, written, err := WriteContext(dir, cx)
	if err != nil {
		return fmt.Errorf("write context: %w", err)
	}
	p.logf("goal %s: %s", goal.Slug, written.Line())
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
		Org: scope.Name(), Project: project.Slug, Repos: dirs, ContextDir: contextDir, ContextFiles: files, Blocked: goal.PausedReason,
		Existing: existing, NextSeq: nextSeq, MeshContext: len(p.MCP) > 0, Evidence: p.evidence(ctx, goal.Slug, scope, project),
	}
	budget, budgetSource := p.budgetWithSource(&goal)
	p.logf("goal %s: planning in %s (%d repos), budget $%.2f (%s)", goal.Slug, dir, len(repos), budget, budgetSource)
	items, reasons, flags, cost, acct, err := p.ask(ctx, goal, scope.NodeID, dir, budget, in, repos, merged)
	acct.SetContextBytes(written.Bytes)
	acct.SetLabel("goal "+goal.Slug, string(harness.KindPlan))
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
	items, reasons, flags, cost, err := p.readPlan(ctx, goal, dir, acct, result, err, repos, merged, in.Evidence)
	return items, reasons, flags, cost, acct, err
}

// readPlan is what follows a planner session, however this process came
// to hold it: its graph read and mapped onto the project's repos, as ask
// describes. A cancelled ctx leaves the session running for the next
// node to adopt; otherwise its session directory goes.
func (p *Planner) readPlan(ctx context.Context, goal gateway.Goal, dir string, acct *SessionAccount, result harness.Result, err error, repos map[string]gateway.Repo, merged []int, evidence []harness.ModelEvidence) ([]gateway.PlannedItem, map[int]string, []string, float64, error) {
	// logged before a done ctx returns: a deadline kills the unit too,
	// and a detach kills nothing
	logKilled(p.logf, "goal "+goal.Slug, result.KilledPerSession(), result.KillError)
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
	flags := unverifiableAcceptance(specs, itemRepo, repos)
	flags = append(flags, modelAgainstEvidence(specs, evidence)...)
	return items, reasons, flags, result.CostUSD, nil
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
		stopFound(p.logf, "goal "+goal.Slug, s)
		return harness.RemoveSession(s.State.Workspace)
	}
	project, repos, err := p.projectRepoMap(ctx, goal, scope)
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
		stopFound(p.logf, "goal "+goal.Slug, s)
		harness.RemoveSession(s.State.Workspace)
		return errNoAttach
	}
	p.logf("goal %s: adopted its %s planner session (pid %d)", goal.Slug, s.Liveness(), s.State.PID)
	result, err := at.Attach(ctx, s.Dir, nil)
	items, reasons, flags, cost, err := p.readPlan(ctx, goal, s.State.Workspace, nil, result, err, repos, merged, p.evidence(ctx, goal.Slug, scope, project))
	if err != nil {
		return err
	}
	if len(flags) > 0 {
		p.logf("goal %s: adopted plan names what no evaluator can see; handing it in: %s", goal.Slug, strings.Join(flags, " | "))
	}
	return p.handIn(ctx, goal, items, reasons, cost, nil)
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
