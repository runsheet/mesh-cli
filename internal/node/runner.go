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
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
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
	// Model is the node's own build model (node.yaml build, else
	// default; "" the CLI's default): what a build session runs on when
	// the item names no model and its agent none (BuildModel).
	Model string
	// Accounts are the node's Claude accounts (node.yaml); a session runs
	// on the first, or on the operator's own login when there are none.
	// They are the fallback to the credentials the platform issues.
	Accounts []AccountConfig
	// Credentials asks the platform for the session's credentials and
	// reports its usage record (nil: node.yaml's accounts, no record).
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
	// Sandbox, when set, runs the session in its own pod with the
	// services its checkout declares started beside it (ADR 007); nil is
	// `sandbox: off`, no services.
	Sandbox *sandbox.Sandbox
	Log     *log.Logger
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
		return ReleaseOnCredential(ctx, r.GW, item.ID, ce, r.logf)
	}
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("workspace: %w", err))
	}

	orgLogin := r.Scopes.LoginOf(item.OrgID)
	cx, err := FetchContext(ctx, r.GW, orgLogin, res.Project.Slug)
	if err != nil {
		r.logf("item %s: context unavailable: %v", label, err)
		cx = &Context{}
	}
	AddLive(ctx, r.GW, cx, item.OrgID, res.Project.ID, r.logf)
	files, err := WriteContext(dir, cx)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("write context: %w", err))
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 || len(cx.Live) > 0 {
		contextDir = ContextDir
	}

	model := BuildModel(ctx, r.GW, item, r.Model)
	r.logf("item %s: build on %s", label, modelName(model))

	mcp := r.mcp(ctx)
	in := harness.PromptInput{
		GoalSlug: res.Goal.Slug, GoalTitle: res.Goal.Title, GoalStatement: res.Goal.Statement, GoalAcceptance: res.Goal.Acceptance,
		Seq: item.Seq, Title: item.Title, Brief: item.Brief, Acceptance: item.Acceptance,
		Repo: res.Repo.FullName(), Branch: spec.Branch, BaseRef: spec.BaseRef, ContextDir: contextDir, ContextFiles: files,
		Attempts: item.Attempts, Evaluation: item.Evaluation, Project: orgLogin + "/" + res.Project.Slug, MeshContext: len(mcp) > 0,
	}
	job := harness.Job{Kind: harness.KindBuild, Workspace: dir, MaxBudgetUSD: r.Budget(res.Goal), Label: label, MCPServers: mcp, Model: model}
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
	acct, err := r.Credentials.Open(ctx, gateway.SessionRef{Kind: string(harness.KindBuild), ItemID: item.ID, GoalID: item.GoalID}, sc.NodeID, r.Accounts, Adapter(r.Harness))
	if errors.Is(err, ErrNoAccount) {
		// no issued account has headroom: the item waits, not fails
		r.logf("item %s: %v; released", label, err)
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, "accounts: "+err.Error()); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	if err != nil {
		return r.fail(ctx, item, err)
	}
	defer unlessDetached(ctx, acct.Cleanup)
	acct.Apply(&job)
	job.Meta = harness.SessionMeta{NodeID: sc.NodeID, ItemID: item.ID, GoalID: item.GoalID, Attempt: item.Attempts, TempDirs: sessionTempDirs(job, acct)}
	// the item's tools, beside the built-in Prologue: a required one this
	// node cannot start hands the item back for a node that can
	runtimes := r.runtimes()
	st := SelectTools(mcp, acct.Tools, string(harness.KindBuild), runtimes)
	if len(st.Missing) > 0 {
		reason := st.DeclineReason()
		r.Declines.Record(item.ID, st.Missing, runtimes, time.Now())
		r.logf("item %s: declined, %s; released", label, reason)
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, reason); rerr != nil {
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
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, reason); rerr != nil {
			r.logf("item %s: release: %v", label, rerr)
		}
		return nil
	}
	pod.Apply(&job)
	in.Services = pod.Prompt()
	job.Prompt = harness.BuildPrompt(in)

	// the session outlives this process: a restart adopts it (ADR 009)
	job.Outlive = true
	b := &buildRun{r: r, item: item, res: res, label: label, spec: spec, dir: dir, cred: cred, acct: acct, runtimes: runtimes, lastText: "session started"}
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

	mu        sync.Mutex
	lastText  string
	sessionID string
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
	if le := (*sandbox.LimitError)(nil); errors.As(runErr, &le) {
		// the node's condition, not the item's failure (ADR 007 Limits)
		reason := LimitReason(label, le)
		b.report(ctx, reason)
		r.Declines.Record(item.ID, []tools.Omission{{Tool: "sandbox:" + le.Limit, Runtime: le.Value}}, b.runtimes, time.Now())
		if r.Condition != nil {
			r.Condition(reason)
		}
		r.logf("item %s: %s", label, reason)
		if rerr := r.GW.ReleaseWorkItem(ctx, item.ID, reason); rerr != nil {
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
	if !result.Success {
		b.report(ctx, "session ended without success: "+result.Text)
		return r.fail(ctx, item, fmt.Errorf("session ended without success: %s", truncate(result.Text, 500)))
	}
	b.report(ctx, result.Text)

	if evidence, ok := harness.ParseAlreadyDone(result.Text); ok {
		// main already holds the work (adr-003): no push, no branch, and
		// the evaluator checks the evidence on the base ref
		if err := b.acct.Carried(r.GW.FinishWorkItemAlreadyDone(ctx, item.ID, truncate(evidence, 4000), b.cost.delta(), b.acct.Report(result))); err != nil {
			return fmt.Errorf("finish %s: %w", label, err)
		}
		r.logf("item %s: finished already done ($%.2f): %s", label, result.CostUSD, truncate(evidence, 200))
		b.endGoal(ctx)
		return nil
	}
	if err := b.cred.Refresh(ctx); err != nil {
		r.logf("item %s: %v", label, err) // the token held may yet serve
	}
	sha, err := r.WS.Push(ctx, b.dir, b.spec.Branch)
	if err != nil {
		return r.fail(ctx, item, fmt.Errorf("push: %w", err))
	}
	if err := b.acct.Carried(r.GW.FinishWorkItem(ctx, item.ID, b.spec.Branch, sha, b.cost.delta(), b.acct.Report(result))); err != nil {
		return fmt.Errorf("finish %s: %w", label, err)
	}
	r.logf("item %s: finished %s@%s ($%.2f)", label, b.spec.Branch, short(sha), result.CostUSD)
	b.endGoal(ctx)
	return nil
}

// endGoal follows a finish: a goal that ended with the item gets its
// final document, and nothing that goes wrong there fails the item.
func (b *buildRun) endGoal(ctx context.Context) {
	r := b.r
	if err := RecordEndedGoal(ctx, r.GW, r.WS, r.WorkRoot, r.Scopes.LoginOf(b.item.OrgID), b.res.Goal.ID, r.logf); err != nil {
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
		s.Stop()
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
