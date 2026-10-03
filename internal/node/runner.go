package node

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"projectmesh.io/mesh-cli/internal/crash"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
	"projectmesh.io/mesh-cli/internal/validate"
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
	// MCP are the MCP servers every session gets (mesh-context).
	MCP map[string]any
	// MCPFunc, when set, is asked for them at every item instead: the
	// node's machine access token expires within the hour, so a build
	// started long after the node did must carry a bearer taken now
	// (channel.Session.MCPFunc is the same seam).
	MCPFunc func(context.Context) map[string]any
	// MaxBudgetUSD caps a session's budget whatever the goal has left.
	MaxBudgetUSD float64
	// Model is the node's own build model (node.yaml build, else
	// default; "" the CLI's default): what a build session runs on when
	// the item names no model and its agent none (BuildModel).
	Model string
	// Credentials asks the platform for the session's credentials and
	// reports its usage record. Required: a nil one is a programming
	// error, and its Open panics (no node.yaml fallback, adr-005).
	Credentials *Credentials
	// ProgressEvery is how often progress is reported (60 s).
	ProgressEvery time.Duration
	// Fallback is the node's one old-platform notice (nil: every item).
	Fallback *TokenFallback
	// Runtimes are the tool runtimes the node has now (Loop.Runtimes;
	// nil: none), which a local tool needs to start.
	Runtimes func() []string
	// Declines remembers an item released for a required tool whose
	// runtime the node lacks, so the loop does not take it again (nil:
	// not remembered).
	Declines *Declines
	// Condition is told the node's condition when a session's sandbox
	// limits kill it (Loop.NoteCondition, the heartbeat's reason); nil
	// tells no one.
	Condition func(reason string)
	// Budgets is node.yaml's `budgets:` in force, read live (ADR 012,
	// Loop.KindModelBudget); nil falls back to Cfg's own, read once at
	// start.
	Budgets func(kind, model string) (amount float64, source string)
	// ContextBudget is node.yaml's `context:` budget in force, read
	// live (Loop.ContextBudget); nil is Cfg's own, read once at start.
	ContextBudget func() ContextConfig
	// Sandbox, when set, runs the session in its own pod with the
	// services its checkout declares started beside it (ADR 007); nil is
	// `sandbox: off`, no services.
	Sandbox *sandbox.Sandbox
	// State keeps what an item's handed-back build spent its budget on
	// for the item's next claim (ADR 012 Hand-back); nil keeps nothing.
	State *RunState
	// HasCommit says whether dir's branch has a commit past origin/base
	// (nil: `git rev-list --count origin/<base>..HEAD`, HasCommitSince),
	// the harness's hand-back test.
	HasCommit func(dir, base string) bool
	// MeshBinary is the mesh executable a session's tool-trim hook runs
	// ("" is `mesh` on the session's PATH).
	MeshBinary string
	// Inspect runs `podman image inspect` for a sandboxed validation's
	// runner image digest (nil: sandbox.ExecRunner).
	Inspect sandbox.Runner
	// Notices say once per run that the platform takes no validation
	// attestations, or no per-step minutes, yet (nil: every item).
	Notices *ValidationNotices
	Log     *log.Logger
}

// toolResults bounds the job's tool results by node.yaml's
// tool_results (trd §3 Tool results): the CLI's limits on its env and,
// for a session on the host, the Bash hook in its settings — the pod has
// no mesh binary to run it.
func (r *Runner) toolResults(job *harness.Job) {
	kb := DefaultToolResultsKB
	if r.Cfg != nil {
		kb = r.Cfg.ToolResults.KB()
	}
	job.Env = append(job.Env, ToolResultsEnv(kb)...)
	if r.Sandbox == nil && job.Session == nil {
		bin := r.MeshBinary
		if bin == "" {
			bin = "mesh"
		}
		job.Settings = ToolTrimSettings(bin, kb)
	}
}

func (r *Runner) runtimes() []string {
	if r.Runtimes == nil {
		return nil
	}
	return r.Runtimes()
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

// kindModelBudget is node.yaml's budgets: figure for kind and model —
// live, through r.Budgets, when set; else Cfg's own, read once at start.
func (r *Runner) kindModelBudget(kind, model string) (float64, string) {
	if r.Budgets != nil {
		return r.Budgets(kind, model)
	}
	return r.Cfg.KindModelBudget(kind, model)
}

// budgetWithSource is Budget with where the figure came from, for the
// session's starting log line.
func (r *Runner) budgetWithSource(goal *gateway.Goal, kind, model string) (float64, string) {
	kindBudget, kindSource := r.kindModelBudget(kind, model)
	return SessionBudget(kindBudget, kindSource, goal, r.MaxBudgetUSD)
}

// Budget is the session's cap (ADR 012): the smallest of the goal's
// remaining budget, node.yaml's budgets: figure for kind and model, and
// MaxBudgetUSD (0 in any of these means "the other one" wins).
func (r *Runner) Budget(goal *gateway.Goal, kind, model string) float64 {
	amount, _ := r.budgetWithSource(goal, kind, model)
	return amount
}

// hasCommit is the job's HasCommit for dir's branch against base.
func (r *Runner) hasCommit(dir, base string) func() bool {
	if r.HasCommit != nil {
		return func() bool { return r.HasCommit(dir, base) }
	}
	return HasCommitSince(dir, base)
}

// costReporter turns cumulative dollars into the cents-since-last-report
// each ReportWorkItemProgress and FinishWorkItem carries.
type costReporter struct {
	mu            sync.Mutex
	reportedCents int
	totalUSD      float64
	// baseUSD is what the sessions before the current one spent: a
	// validation retry's turn counts its own dollars from zero (carry).
	baseUSD float64
}

func (c *costReporter) observe(usd float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if usd += c.baseUSD; usd > c.totalUSD {
		c.totalUSD = usd
	}
}

// carry starts the next session's dollars on top of the total so far.
func (c *costReporter) carry() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseUSD = c.totalUSD
}

// total is every session's dollars so far.
func (c *costReporter) total() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalUSD
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
// session, progress, push, finish — or fail, or release it when GitHub
// refuses the item's token through every retry (ReleaseOnCredential). A
// cancelled ctx (the node's own shutdown) leaves the item as it is for a
// retry.
func (r *Runner) RunItem(ctx context.Context, item gateway.WorkItem) error {
	res, err := Resolve(ctx, r.GW, item)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("resolve: %w", err))
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	r.logf("item %s: %q on %s from %s", label, item.Title, res.Repo.FullName(), res.BaseRef())

	spec := WorkspaceSpec{Root: r.WorkRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: res.Branch(), BaseRef: res.BaseRef()}
	// the item's own GitHub token, on the checkout before it is cloned
	cred, err := NewItemCred(ctx, r.GW, r.WS, r.Scopes, item, spec, r.Fallback, r.logf)
	if err != nil {
		return r.fail(ctx, item, err)
	}
	dir, err := PrepareWithRetry(ctx, r.WS, spec, cred, r.logf)
	if ce := (*CredentialError)(nil); errors.As(err, &ce) {
		sc, _ := r.Scopes.ByOrg(item.OrgID)
		return ReleaseOnCredential(ctx, r.GW, item.ID, sc.NodeID, ce, r.logf)
	}
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("workspace: %w", err))
	}

	orgName := r.Scopes.NameOf(item.OrgID)
	cx, err := FetchContext(ctx, r.GW, orgName, res.Project.Slug, ContextOptions{Budget: r.contextBudget(), Repos: []string{res.Repo.FullName()}, Checkouts: map[string]string{res.Repo.FullName(): dir}, Goal: res.Goal, Logf: r.logf})
	if err != nil {
		r.logf("item %s: context unavailable: %v", label, err)
		cx = &Context{}
	}
	files, written, err := WriteContext(dir, cx)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("write context: %w", err))
	}
	r.logf("item %s: %s", label, written.Line())
	if w := written.TargetWarning(); w != "" {
		r.logf("item %s: %s", label, w)
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 || cx.Live != nil {
		contextDir = ContextDir
	}

	model := BuildModel(ctx, r.GW, item, r.Model)
	budget, budgetSource := r.budgetWithSource(res.Goal, "build", model)
	r.logf("item %s: build on %s, budget $%.2f (%s)", label, modelName(model), budget, budgetSource)

	// the clauses the last evaluation failed, so the retry works through a
	// list instead of mining them out of Evaluation's prose. Unread records
	// are not fatal: the build runs on the prose alone, as it did before.
	var failed []harness.FailedClause
	if item.Attempts > 0 {
		was, err := r.GW.WorkItemPriorAttempts(ctx, item.ID)
		if err != nil {
			r.logf("item %s: earlier evaluations unread, building on the notes alone: %v", label, err)
		}
		latest := gateway.PriorAttempt{}
		for _, p := range was {
			if p.Attempt >= latest.Attempt {
				latest = p
			}
		}
		for _, c := range latest.Clauses {
			failed = append(failed, harness.FailedClause{Clause: c.Clause, Satisfy: c.Satisfy})
		}
	}
	mcp := r.mcp(ctx)
	in := harness.PromptInput{
		GoalSlug: res.Goal.Slug, GoalTitle: res.Goal.Title, GoalStatement: res.Goal.Statement, GoalAcceptance: res.Goal.Acceptance,
		Seq: item.Seq, ItemID: item.ID, Title: item.Title, Brief: item.Brief, Acceptance: item.Acceptance,
		Repo: res.Repo.FullName(), Branch: spec.Branch, BaseRef: spec.BaseRef, ContextDir: contextDir, ContextFiles: files,
		Attempts: item.Attempts, Evaluation: item.Evaluation, FailedClauses: failed, Project: orgName + "/" + res.Project.Slug, MeshContext: len(mcp) > 0,
		LastSession: r.State.LastSession(item.ID),
	}
	job := harness.Job{Kind: harness.KindBuild, Workspace: dir, MaxBudgetUSD: budget, Label: label, MCPServers: mcp, Model: model}
	// past 60% of its budget with nothing committed the session is handed
	// back for one commit-and-finish turn (ADR 012 Hand-back)
	job.HandBackAt = harness.DefaultHandBackAt
	job.HasCommit = r.hasCommit(dir, spec.BaseRef)
	if tok := cred.Token(); tok != nil {
		ghDir, err := NewGHConfigDir()
		if err != nil {
			return r.fail(ctx, item, fmt.Errorf("gh config dir: %w", err))
		}
		// a session left running for the next node keeps its dirs: the
		// node that ends it removes them (TempDirs)
		defer unlessDetached(ctx, func() { os.RemoveAll(ghDir) })
		job.Env = SessionEnv(tok, ghDir)
	}
	sc, _ := r.Scopes.ByOrg(item.OrgID)
	acct, err := r.Credentials.Open(withSubject(ctx, sc, cmp.Or(res.Project.ID, item.ProjectID), res.Project.Slug), gateway.SessionRef{Kind: string(harness.KindBuild), ItemID: item.ID, GoalID: item.GoalID}, sc.NodeID, Adapter(r.Harness))
	if nc, ok := NoCredentialOf(err); ok {
		// nothing issued for the adapter: the item waits, never on the
		// machine's own login; the reason is said once for the goal
		// (NoCredentials.Say), not on every release
		r.logf("item %s: no credential for %s; released", label, nc.Adapter)
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, sc.NodeID, "accounts: "+nc.Error()); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	if errors.Is(err, ErrNoAccount) {
		// no issued account has headroom: the item waits, not fails; an
		// ErrNoHeadroom's reason is said once for the goal
		// (NoCredentials.NoteHeadroom), not on every release
		if nh, ok := NoHeadroomOf(err); ok {
			r.logf("item %s: no issued %s credential has headroom; released", label, nh.Adapter)
		} else {
			r.logf("item %s: %v; released", label, err)
		}
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, sc.NodeID, "accounts: "+err.Error()); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	if err != nil {
		return r.fail(ctx, item, err)
	}
	defer unlessDetached(ctx, acct.Cleanup)
	acct.SetContextBytes(written.Bytes)
	acct.SetLabel("item "+label, string(harness.KindBuild))
	acct.Apply(&job)
	job.Meta = harness.SessionMeta{NodeID: sc.NodeID, ItemID: item.ID, GoalID: item.GoalID, Attempt: item.Attempts, TempDirs: sessionTempDirs(job, acct)}
	// the item's tools, beside the built-in mesh-context: a required one this
	// node cannot start hands the item back for a node that can
	runtimes := r.runtimes()
	st := SelectTools(mcp, acct.Tools, tools.RoleFor(string(harness.KindBuild)), runtimes)
	if len(st.Missing) > 0 {
		reason := st.DeclineReason()
		r.Declines.Record(item.ID, st.Missing, runtimes, time.Now())
		r.logf("item %s: declined, %s; released", label, reason)
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, sc.NodeID, reason); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	if err := st.UseTools(&job, mcp, acct); err != nil {
		return r.fail(ctx, item, fmt.Errorf("tools: %w", err))
	}
	if len(st.Use) > 0 {
		r.logf("item %s: tools %s", label, st.Names())
	}
	// the checkout's services, started in the session's pod before its
	// agent; the pod and its sidecars go on every way out
	pod, err := OpenSessionPod(ctx, r.Sandbox, dir, label, harness.KindBuild, r.logf)
	defer pod.Close()
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("services: %w", err))
	}
	if _, failed := pod.RequiredFailure(); failed {
		// a required service that does not come up hands the item back,
		// and no agent starts
		reason := pod.ReleaseReason()
		r.Declines.Record(item.ID, pod.Declined(), runtimes, time.Now())
		r.logf("item %s: %s; released", label, reason)
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, sc.NodeID, reason); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	pod.Apply(&job)
	r.toolResults(&job)
	in.Services = pod.Prompt()
	job.Prompt = harness.BuildPrompt(in)

	// the session outlives this process: a restart adopts it (ADR 009)
	job.Outlive = true
	b := &buildRun{r: r, item: item, res: res, label: label, spec: spec, dir: dir, cred: cred, acct: acct, runtimes: runtimes, lastText: "session started", job: &job, pod: pod, started: time.Now()}
	b.report(ctx, "session started: "+spec.Branch+" on "+modelName(model))
	if note := st.OmittedNote(); note != "" {
		r.logf("item %s: %s", label, note)
		b.report(ctx, note)
	}
	if note := pod.MissingNote(); note != "" {
		r.logf("item %s: %s", label, note)
		b.report(ctx, note)
	}
	result, runErr := b.run(ctx, func(onProgress func(harness.Progress)) (harness.Result, error) {
		return r.Harness.Run(ctx, job, onProgress)
	})
	// the usage report rides the finish (afterBuild); Finish logs it when
	// nothing carried it
	defer acct.Finish(ctx, result)
	return r.afterBuild(ctx, b, result, runErr)
}

// buildRun is one build session as its tail needs it, whether this
// process started the session (RunItem) or adopted it (AdoptItem).
type buildRun struct {
	r        *Runner
	item     gateway.WorkItem
	res      *Resolved
	label    string
	spec     WorkspaceSpec
	dir      string
	cred     *ItemCred
	acct     *SessionAccount // nil for an adopted session
	runtimes []string
	cost     costReporter
	// job is the session's job, for a validation retry's turn; pod its
	// sandbox, where the validation runs; started when it began. The job
	// and pod are nil for an adopted session.
	job     *harness.Job
	pod     *SessionPod
	started time.Time

	mu        sync.Mutex
	lastText  string
	sessionID string
}

// session is the session's id as last read.
func (b *buildRun) session() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// podSession is the session's sandbox (nil: none, or adopted).
func (b *buildRun) podSession() *sandbox.Session {
	if b.pod == nil {
		return nil
	}
	return b.pod.Session
}

// report is the item's heartbeat: the note (the session's last text when
// empty), the cents since the last one and the session id.
func (b *buildRun) report(ctx context.Context, note string) {
	b.mu.Lock()
	n, sid := note, b.sessionID
	if n == "" {
		n = b.lastText
	}
	b.mu.Unlock()
	if err := b.r.GW.ReportWorkItemProgress(ctx, b.item.ID, truncate(n, 2000), b.cost.delta(), sid); err != nil {
		b.r.logf("item %s: progress: %v", b.label, err)
	}
}

// reportToolUse notes on the item the served tools the session never
// called or whose calls errored (SessionAccount.UseNote); nothing when
// each was called cleanly, or for an adopted session.
func (b *buildRun) reportToolUse(ctx context.Context, result harness.Result) {
	if note := b.acct.UseNote(result); note != "" {
		b.r.logf("item %s: %s", b.label, note)
		b.report(ctx, note)
	}
}

// run runs the session through start — Harness.Run or Attach — with
// the item's heartbeat every ProgressEvery and its token renewed.
func (b *buildRun) run(ctx context.Context, start func(onProgress func(harness.Progress)) (harness.Result, error)) (harness.Result, error) {
	every := b.r.ProgressEvery
	if every <= 0 {
		every = 60 * time.Second
	}
	tickCtx, stopTicker := context.WithCancel(ctx)
	defer stopTicker()
	go func() {
		defer crash.Guard()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-tickCtx.Done():
				return
			case <-t.C:
				b.report(tickCtx, "")
				// a session may outlive its token's hour
				if err := b.cred.Refresh(tickCtx); err != nil && tickCtx.Err() == nil {
					b.r.logf("item %s: %v", b.label, err)
				}
			}
		}
	}()
	return start(func(p harness.Progress) {
		b.mu.Lock()
		if p.Text != "" {
			b.lastText = b.acct.Redact(p.Text)
		}
		if p.SessionID != "" {
			b.sessionID = p.SessionID
		}
		b.mu.Unlock()
		if p.CostUSD > 0 {
			b.cost.observe(p.CostUSD)
		}
	})
}

// afterBuild is what follows a build session, however this process came
// to hold it: the item released on a sandbox limit, failed on an error
// or an unsuccessful end, finished already done (no push: main holds the
// work), or pushed and finished. A cancelled ctx — the
// node shutting down — leaves the item and its still-running session
// for the next node to adopt. Once the item is finished or failed the
// session directory goes.
func (r *Runner) afterBuild(ctx context.Context, b *buildRun, result harness.Result, runErr error) error {
	item, label := b.item, b.label
	// what the agent left running in the session's unit, killed at its
	// end (ADR 008, The session's end): a line in the log — logged before
	// a done ctx returns, since a deadline kills the unit too — and a
	// note on the item. A detach kills nothing.
	killedNote := logKilled(r.logf, "item "+label, result.KilledPerSession(), result.KillError)
	if ctx.Err() != nil {
		r.logf("item %s: node shutting down; its session is left for the next node to adopt", label)
		return ctx.Err()
	}
	defer harness.RemoveSession(b.dir)
	b.cost.observe(result.CostUSD)
	if result.SessionID != "" {
		b.mu.Lock()
		b.sessionID = result.SessionID
		b.mu.Unlock()
	}
	if killedNote != "" {
		b.report(ctx, killedNote)
	}
	if le := (*sandbox.LimitError)(nil); errors.As(runErr, &le) {
		// the node's condition, not the item's failure (ADR 007 Limits)
		reason := LimitReason(label, le)
		b.report(ctx, reason)
		r.Declines.Record(item.ID, []tools.Omission{{Tool: "sandbox:" + le.Limit, Runtime: le.Value}}, b.runtimes, time.Now())
		if r.Condition != nil {
			r.Condition(reason)
		}
		r.logf("item %s: %s", label, reason)
		bsc, _ := r.Scopes.ByOrg(b.item.OrgID)
		if rerr := r.GW.ReleaseWorkItem(ctx, b.item.ID, bsc.NodeID, reason); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	if runErr != nil {
		runErr = b.acct.RedactErr(runErr)
		b.report(ctx, "harness error: "+runErr.Error())
		return r.fail(ctx, item, runErr)
	}
	result.Text = b.acct.Redact(result.Text)
	b.handedBack(ctx, result)
	if !result.Success {
		b.report(ctx, "session ended without success: "+result.Text)
		b.reportToolUse(ctx, result)
		return r.fail(ctx, item, fmt.Errorf("session ended without success: %s", truncate(result.Text, 500)))
	}
	b.report(ctx, result.Text)
	b.reportToolUse(ctx, result)

	// the session's end is its mesh_finish or mesh_fail call when it made
	// one (ADR-023): the final message need not be formatted, or there at
	// all; with neither, the text is parsed as before and that is logged
	fin := finishCallOf(result)
	if fin.fail != "" {
		return r.fail(ctx, item, errors.New(b.acct.Redact(fin.fail)))
	}
	if !fin.called {
		r.logf("item %s: no mesh_finish call; parsed the final message", label)
	}
	evidence, already := fin.evidence, fin.evidence != ""
	if !fin.called {
		evidence, already = harness.ParseAlreadyDone(result.Text)
	}
	if already {
		// main already holds the work (adr-003): no push, no branch, and
		// the evaluator checks the evidence on the base ref
		if err := b.acct.Carried(r.GW.FinishWorkItemAlreadyDone(ctx, item.ID, truncate(b.acct.Redact(evidence), 4000), b.cost.delta(), b.acct.Report(result))); err != nil {
			if fin.handedIn(err) {
				return r.finishedByTool(ctx, b)
			}
			return fmt.Errorf("finish %s: %w", label, err)
		}
		r.logf("item %s: finished already done ($%.2f): %s", label, result.CostUSD, truncate(evidence, 200))
		b.endGoal(ctx)
		return nil
	}
	// the repository's validation on the commit the item finishes with
	// (runsheet ADR 032): a failing step gets the builder one more turn,
	// then fails the item naming it
	v, err := r.startValidation(ctx, b)
	if err != nil {
		b.report(ctx, "validation: "+err.Error())
		return r.fail(ctx, item, fmt.Errorf("validation: %w", err))
	}
	var att validate.Attestation
	if v != nil {
		defer v.close()
		att, err = r.validateBuild(ctx, b, v, result)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if rep := b.acct.Report(result); rep != nil {
			rep.Validation = v.usage()
		}
		var vf *validationFailure
		if errors.As(err, &vf) {
			if len(v.runs) > 0 {
				r.uploadAttestation(ctx, b, v.runs[len(v.runs)-1])
			}
			b.report(ctx, vf.Error())
			return r.fail(ctx, item, vf)
		}
		if err != nil {
			err = b.acct.RedactErr(err)
			b.report(ctx, "validation: "+err.Error())
			return r.fail(ctx, item, fmt.Errorf("validation: %w", err))
		}
	}
	if err := b.cred.Refresh(ctx); err != nil {
		r.logf("item %s: %v", label, err) // the token held may yet serve
	}
	sha, err := r.WS.Push(ctx, b.dir, b.spec.Branch)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("push: %w", err))
	}
	if v != nil {
		// uploaded after the push: the platform signs for a commit that exists
		if att.Commit != sha {
			r.logf("item %s: validation ran on %s, the push is %s", label, short(att.Commit), short(sha))
		}
		r.uploadAttestation(ctx, b, att)
	}
	if err := b.acct.Carried(r.validationUsage(r.GW.FinishWorkItem(ctx, item.ID, b.spec.Branch, sha, b.cost.delta(), b.acct.Report(result)))); err != nil {
		if fin.handedIn(err) {
			return r.finishedByTool(ctx, b)
		}
		return fmt.Errorf("finish %s: %w", label, err)
	}
	r.logf("item %s: finished %s@%s ($%.2f)", label, b.spec.Branch, short(sha), result.CostUSD)
	b.endGoal(ctx)
	return nil
}

// finishCall is a build session's end as its mesh-work tool calls say
// it (ADR-023, sessions finish by tool call): called when it called
// mesh_finish — evidence set for outcome already_done — and fail the
// error of a mesh_fail call made with no mesh_finish.
type finishCall struct {
	called   bool
	evidence string
	fail     string
}

// finishCallOf reads result.WorkCalls for the session's finish. A
// mesh_finish with outcome already_done and no evidence is no finish:
// the final message is parsed for it instead. The branch and head_sha
// the session names are not trusted: the node pushes its own checkout
// and hands in the head it pushed.
func finishCallOf(result harness.Result) finishCall {
	if in, ok := result.WorkCalls["mesh_finish"]; ok {
		if workArg(in, "outcome") != gateway.OutcomeAlreadyDone {
			return finishCall{called: true}
		}
		if ev := workArg(in, "evidence"); ev != "" {
			return finishCall{called: true, evidence: ev}
		}
		return finishCall{}
	}
	if in, ok := result.WorkCalls["mesh_fail"]; ok {
		msg := workArg(in, "error")
		if msg == "" {
			msg = cmp.Or(workArg(in, "reason"), "no reason given")
		}
		return finishCall{called: true, fail: "mesh_fail: " + msg}
	}
	return finishCall{}
}

// workArg is a work tool input's string argument, trimmed ("" when it is
// absent or not a string).
func workArg(in map[string]any, key string) string {
	v, _ := in[key].(string)
	return strings.TrimSpace(v)
}

// handedIn says whether the node's own finish, refused with err, was
// refused because the session's mesh_finish call already moved the item
// on the platform: a refusal (never a transport failure or an older
// platform's unknown field) after a call.
func (f finishCall) handedIn(err error) bool {
	return f.called && gateway.IsRefusal(err) && !gateway.IsUnsupported(err)
}

// finishedByTool is a build the platform's hosted mesh_finish already
// handed in: the node's own finish was refused for the item's state, so
// the build is done, not failed.
func (r *Runner) finishedByTool(ctx context.Context, b *buildRun) error {
	r.logf("item %s: finished by tool call on the platform", b.label)
	b.endGoal(ctx)
	return nil
}

// handedBack records what a handed-back session spent its budget on
// under the item, for its next claim's prompt, and notes it on the item
// before the finish (ADR 012 Hand-back); a session that ran to its end
// clears the record. A handed-back build that committed finishes as any
// other; one that still did not is pushed and finished empty, which
// the platform routes back to ready (empties-go-back-to-the-builder).
func (b *buildRun) handedBack(ctx context.Context, result harness.Result) {
	r := b.r
	if !result.HandedBack {
		if result.Success {
			r.State.ClearLastSession(b.item.ID)
		}
		return
	}
	spent := b.acct.Redact(result.SpentOn)
	if err := r.State.RecordLastSession(b.item.ID, spent); err != nil {
		r.logf("item %s: last session: %v", b.label, err)
	}
	note := "handed back with nothing committed: " + spent
	r.logf("item %s: %s", b.label, note)
	b.report(ctx, note)
}

// endGoal follows a finish: a goal that ended with the item gets its
// final document, and nothing that goes wrong there fails the item.
func (b *buildRun) endGoal(ctx context.Context) {
	r := b.r
	if err := RecordEndedGoal(ctx, r.GW, r.WS, r.WorkRoot, r.Scopes.NameOf(b.item.OrgID), b.res.Goal.ID, r.logf); err != nil {
		r.logf("item %s: goal doc: %v", b.label, err)
	}
}

// AdoptItem takes up a build session a node process before this one
// started (ADR 009): the item is not claimed again and no session is
// started; the one in s is followed from its saved offset — or, gone,
// read from its file — with the heartbeat and token renewal RunItem
// gives its own, and then afterBuild, as RunItem ends. An item the
// platform no longer has running on this node is not this session's to
// finish: its session is stopped and its directory removed.
func (r *Runner) AdoptItem(ctx context.Context, item gateway.WorkItem, s FoundSession) error {
	if item.Status != "claimed" && item.Status != "running" || item.NodeID != s.State.NodeID {
		r.logf("item %s: adopted session is no longer this node's (%s); stopped", item.ID, item.Status)
		stopFound(r.logf, "item "+item.ID, s)
		return harness.RemoveSession(s.State.Workspace)
	}
	res, err := Resolve(ctx, r.GW, item)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("resolve: %w", err))
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	spec := WorkspaceSpec{Root: r.WorkRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: res.Branch(), BaseRef: res.BaseRef()}
	// the item's token on the checkout again: the session's pushes, and
	// the node's, need a live one
	cred, err := NewItemCred(ctx, r.GW, r.WS, r.Scopes, item, spec, r.Fallback, r.logf)
	if err != nil {
		return r.fail(ctx, item, err)
	}
	at, ok := r.Harness.(harness.Attacher)
	if !ok {
		return r.fail(ctx, item, errNoAttach)
	}
	b := &buildRun{r: r, item: item, res: res, label: label, spec: spec, dir: s.State.Workspace, cred: cred, runtimes: r.runtimes(), lastText: "session adopted", sessionID: s.State.SessionID}
	r.logf("item %s: adopted its %s session (pid %d, from byte %d)", label, s.Liveness(), s.State.PID, s.State.Offset)
	result, runErr := b.run(ctx, func(onProgress func(harness.Progress)) (harness.Result, error) {
		return at.Attach(ctx, s.Dir, onProgress)
	})
	return r.afterBuild(ctx, b, result, runErr)
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

// contextBudget is the context budget for r's sessions: live, through
// r.ContextBudget, when set; else Cfg's own.
func (r *Runner) contextBudget() ContextConfig {
	if r.ContextBudget != nil {
		return r.ContextBudget()
	}
	if r.Cfg == nil {
		return ContextConfig{}.Effective()
	}
	return r.Cfg.Context.Effective()
}
