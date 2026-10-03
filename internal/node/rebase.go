package node

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"
	"projectmesh.io/mesh-cli/internal/tools"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// Rebaser is a node's session of kind rebase (runsheet ADR 008 step 2,
// trd §6a): an item whose pull request conflicts with its base in a way
// the integrator could not resolve itself, claimed with
// ClaimWorkItemRebase. The session checks out the branch in the item's
// own checkout, rebases it onto the base keeping both sides' intent,
// re-runs the acceptance and pushes with --force-with-lease; the node
// reads the pushed head back and records it with FinishWorkItemRebase —
// or gives the rebase up with FailWorkItemRebase and the reason, which
// hands the item to its builder. Every way out ends the claim: a claim
// left held waits out the platform's 15-minute stall timer.
type Rebaser struct {
	GW       Gateway
	Harness  harness.Harness
	WS       Workspacer
	Cfg      *Config
	Scopes   Scopes
	WorkRoot string
	// MCP are the MCP servers the session gets (mesh-context).
	MCP map[string]any
	// MaxBudgetUSD caps the session (5 by default).
	MaxBudgetUSD float64
	// Budgets is node.yaml's `budgets:` in force, read live (ADR 012,
	// Loop.KindModelBudget); nil falls back to Cfg's own, read once at
	// start.
	Budgets func(kind, model string) (amount float64, source string)
	// ContextBudget is node.yaml's `context:` budget in force, read
	// live (Loop.ContextBudget); nil is Cfg's own, read once at start.
	ContextBudget func() ContextConfig
	// Model is the node's own rebase model (node.yaml rebase, else
	// default; "" the CLI's default): what a rebase session runs on when
	// the item names no model and its agent none (BuildModel, the
	// builder's choice).
	Model string
	// Credentials asks the platform for the session's credentials and
	// reports its usage record. Required: a nil one is a programming
	// error, and its Open panics (no node.yaml fallback, adr-005).
	Credentials *Credentials
	// Timeout caps its wall clock (0 none).
	Timeout time.Duration
	// ProgressEvery is how often the rebase's heartbeat is sent (60 s):
	// ReportWorkItemProgress while the rebase is claimed, with the
	// session's cents since the last one.
	ProgressEvery time.Duration
	// Fallback is the node's one old-platform notice (nil: every item).
	Fallback *TokenFallback
	// Sandbox, when set, runs the session in its own pod with the
	// services its checkout declares (as Runner.Sandbox): the acceptance
	// is re-run on the rebased head.
	Sandbox *sandbox.Sandbox
	// Inspect runs `podman image inspect` for a sandboxed validation's
	// image digest (nil: the real podman), as Runner.Inspect.
	Inspect sandbox.Runner
	// Notices say once per run that the platform takes no attestations,
	// shared with the node's Runner.
	Notices *ValidationNotices
	Log     *log.Logger
}

func (r *Rebaser) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Printf(format, args...)
	}
}

// kindModelBudget is node.yaml's budgets: figure for kind and model —
// live, through r.Budgets, when set; else Cfg's own, read once at start.
func (r *Rebaser) kindModelBudget(kind, model string) (float64, string) {
	if r.Budgets != nil {
		return r.Budgets(kind, model)
	}
	return r.Cfg.KindModelBudget(kind, model)
}

// budgetWithSource is the rebase's cost cap (ADR 012) and where it came
// from, for the session's starting log line.
func (r *Rebaser) budgetWithSource(goal *gateway.Goal, model string) (float64, string) {
	kindBudget, kindSource := r.kindModelBudget("rebase", model)
	fallback := r.MaxBudgetUSD
	if fallback <= 0 {
		fallback = 5
	}
	return SessionBudget(kindBudget, kindSource, goal, fallback)
}

// Rebase runs one claimed rebase to its end. The error is what went
// wrong, already handed in as the rebase's failure when it could be.
func (r *Rebaser) Rebase(ctx context.Context, item gateway.WorkItem) error {
	// a rebase is build-side: its commands act as the builder (ADR-024)
	ctx = actorContext(ctx, r.Cfg, gateway.RoleBuilder)
	rb := &rebaseRun{r: r, item: item, label: item.ID} // the goal's slug and the seq once resolved
	fail := func(cause error) error { return rb.fail(ctx, cause) }

	res, err := Resolve(ctx, r.GW, item)
	if err != nil {
		return fail(fmt.Errorf("resolve: %w", err))
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	rb.label = label
	branch := item.Branch
	if branch == "" {
		branch = res.Branch()
	}
	spec := WorkspaceSpec{Root: r.WorkRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: branch, BaseRef: res.BaseRef()}
	// the item's own GitHub token: the push is the session's whole point
	cred, err := NewItemCred(ctx, r.GW, r.WS, r.Scopes, item, spec, r.Fallback, r.logf)
	if err != nil {
		return fail(err)
	}
	dir, err := r.WS.Prepare(ctx, spec)
	if err != nil {
		return fail(fmt.Errorf("workspace: %w", err))
	}
	// the rebased commits are the agent's, with the pairers' trailers
	// (ADR-024)
	ident := identityFor(r.Cfg, tools.RoleFor(string(harness.KindRebase)), item, res.Goal)
	if err := r.WS.Identify(ctx, dir, ident); err != nil {
		return fail(fmt.Errorf("workspace: %w", err))
	}
	orgName := r.Scopes.NameOf(item.OrgID)
	cx, err := FetchContext(ctx, r.GW, orgName, res.Project.Slug, ContextOptions{Budget: r.contextBudget(), Repos: []string{res.Repo.FullName()}, Checkouts: map[string]string{res.Repo.FullName(): dir}, Goal: res.Goal, Logf: r.logf})
	if err != nil {
		r.logf("item %s: context unavailable: %v", label, err)
		cx = &Context{}
	}
	files, written, err := WriteContext(dir, cx)
	if err != nil {
		return fail(fmt.Errorf("write context: %w", err))
	}
	r.logf("item %s: %s", label, written.Line())
	if w := written.TargetWarning(); w != "" {
		r.logf("item %s: %s", label, w)
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 || cx.Live != nil {
		contextDir = ContextDir
	}
	in := harness.RebaseInput{
		GoalSlug: res.Goal.Slug, GoalTitle: res.Goal.Title, Seq: item.Seq, Title: item.Title, Brief: item.Brief, Acceptance: item.Acceptance,
		Repo: res.Repo.FullName(), Branch: branch, BaseRef: spec.BaseRef, Conflict: item.RebaseConflict, Files: item.RebaseFiles,
		ContextDir: contextDir, ContextFiles: files, Project: orgName + "/" + res.Project.Slug, MeshContext: len(r.MCP) > 0,
	}
	model := BuildModel(ctx, r.GW, item, r.Model)
	budget, budgetSource := r.budgetWithSource(res.Goal, model)
	r.logf("item %s: rebase budget $%.2f (%s)", label, budget, budgetSource)
	job := harness.Job{Kind: harness.KindRebase, Workspace: dir, MaxBudgetUSD: budget, Label: label + " rebase", Timeout: r.Timeout, MCPServers: r.MCP, Model: model}
	// the checkout's services, started in the session's pod before its
	// agent; the pod and its sidecars go on every way out. A required one
	// that does not come up gives the rebase up, naming it.
	pod, err := OpenSessionPod(ctx, r.Sandbox, dir, job.Label, harness.KindRebase, ident.Sandbox(dir), r.logf)
	defer pod.Close()
	if err != nil {
		return fail(fmt.Errorf("services: %w", err))
	}
	if _, failed := pod.RequiredFailure(); failed {
		return fail(errors.New(pod.ReleaseReason()))
	}
	pod.Apply(&job)
	in.Services = pod.Prompt()
	job.Prompt = harness.RebasePrompt(in)
	if tok := cred.Token(); tok != nil {
		ghDir, err := NewGHConfigDir()
		if err != nil {
			return fail(fmt.Errorf("gh config dir: %w", err))
		}
		// a session left running for the next node keeps its dirs: the
		// node that ends it removes them (TempDirs)
		defer unlessDetached(ctx, func() { os.RemoveAll(ghDir) })
		job.Env = SessionEnv(tok, ghDir)
	}
	ident.Apply(&job)
	sc, _ := r.Scopes.ByOrg(item.OrgID)
	acct, err := r.Credentials.Open(withSubject(ctx, sc, cmp.Or(res.Project.ID, item.ProjectID), res.Project.Slug), gateway.SessionRef{Kind: string(harness.KindRebase), ItemID: item.ID, GoalID: item.GoalID}, sc.NodeID, Adapter(r.Harness))
	if isNoCredential(err) {
		// a wait, not a failure: the conflict is no reason to hand the
		// item to its builder. The platform has no release for a rebase
		// claim, so the claim lapses on its 15-minute timer and the loop
		// leaves the project's rebases unclaimed meanwhile
		// (Loop.NoCredential); node.log has said why, once for the goal
		// (NoCredentials.Note), so nothing here says it again.
		return err
	}
	if err != nil {
		return fail(err)
	}
	acct.SetContextBytes(written.Bytes)
	acct.SetLabel("item "+label, string(harness.KindRebase))
	defer unlessDetached(ctx, acct.Cleanup)
	acct.Apply(&job)
	job.Meta = harness.SessionMeta{NodeID: sc.NodeID, ItemID: item.ID, GoalID: item.GoalID, Attempt: item.Attempts, TempDirs: sessionTempDirs(job, acct)}

	rb.cred, rb.dir, rb.branch, rb.base = cred, dir, branch, spec.BaseRef
	rb.job, rb.pod = &job, pod
	r.logf("item %s: rebase on %s: %s onto %s", label, modelName(model), branch, spec.BaseRef)
	rb.report(ctx, "rebase session started: "+branch+" onto "+spec.BaseRef+" on "+modelName(model))
	if note := pod.MissingNote(); note != "" {
		r.logf("item %s: %s", label, note)
		rb.report(ctx, note)
	}
	// the session outlives this process: a restart adopts it (ADR 009)
	job.Outlive = true
	result, runErr := rb.run(ctx, func(onProgress func(harness.Progress)) (harness.Result, error) {
		return r.Harness.Run(ctx, job, onProgress)
	})
	// the usage report rides the finish (afterRebase); Finish logs it
	// when nothing carried it
	defer acct.Finish(ctx, result)
	return r.afterRebase(ctx, rb, acct, result, runErr)
}

// rebaseRun is one rebase session as its tail needs it, whether this
// process started it (Rebase) or adopted it (AdoptRebase).
type rebaseRun struct {
	r            *Rebaser
	item         gateway.WorkItem
	label        string
	cred         *ItemCred
	dir          string
	branch, base string
	cost         costReporter
	// job and pod are the session's, for the rebased head's validation:
	// nil for an adopted session (its pod ended with the node before it)
	job *harness.Job
	pod *SessionPod

	mu        sync.Mutex
	sessionID string
}

// podSession is the session's sandbox (nil: none, or adopted).
func (rb *rebaseRun) podSession() *sandbox.Session {
	if rb.pod == nil {
		return nil
	}
	return rb.pod.Session
}

func (rb *rebaseRun) sid() string {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.sessionID
}

// report is the rebase's heartbeat, with the session's cents since the
// last one: finish and fail carry none.
func (rb *rebaseRun) report(ctx context.Context, note string) {
	if err := rb.r.GW.ReportWorkItemProgress(ctx, rb.item.ID, truncate(note, 2000), rb.cost.delta(), rb.sid()); err != nil {
		rb.r.logf("item %s: rebase progress: %v", rb.label, err)
	}
}

// fail gives the rebase up with the cause, handing the item to its
// builder.
func (rb *rebaseRun) fail(ctx context.Context, cause error) error {
	// a node stopping before the session starts still hands the rebase
	// back, rather than holding it until the platform's timer does
	fctx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		fctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
	}
	reason := truncate(cause.Error(), 4000)
	rb.r.logf("item %s: rebase failed: %s", rb.label, reason)
	rb.report(fctx, "rebase failed")
	if err := rb.r.GW.FailWorkItemRebase(fctx, rb.item.ID, reason); err != nil {
		rb.r.logf("item %s: fail rebase: %v", rb.label, err)
	}
	return cause
}

// run runs the session through start — Harness.Run or Attach — with the
// rebase's heartbeat every ProgressEvery and its token renewed.
func (rb *rebaseRun) run(ctx context.Context, start func(onProgress func(harness.Progress)) (harness.Result, error)) (harness.Result, error) {
	every := rb.r.ProgressEvery
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
				rb.report(tickCtx, "rebasing "+rb.branch)
				if err := rb.cred.Refresh(tickCtx); err != nil && tickCtx.Err() == nil {
					rb.r.logf("item %s: %v", rb.label, err)
				}
			}
		}
	}()
	return start(func(p harness.Progress) {
		if p.SessionID != "" {
			rb.mu.Lock()
			rb.sessionID = p.SessionID
			rb.mu.Unlock()
		}
		if p.CostUSD > 0 {
			rb.cost.observe(p.CostUSD)
		}
	})
}

// afterRebase is what follows a rebase session, however this process
// came to hold it: the pushed head read back and finished, or the
// rebase failed with the reason. A cancelled ctx — the node shutting
// down — leaves the claim and the still-running session for the next
// node to adopt; otherwise the session directory goes.
func (r *Rebaser) afterRebase(ctx context.Context, rb *rebaseRun, acct *SessionAccount, result harness.Result, runErr error) error {
	// logged before a done ctx returns: a deadline kills the unit too
	killedNote := logKilled(r.logf, "item "+rb.label, result.KilledPerSession(), result.KillError)
	if ctx.Err() != nil {
		r.logf("item %s: node shutting down; its rebase session is left for the next node to adopt", rb.label)
		return ctx.Err()
	}
	defer harness.RemoveSession(rb.dir)
	fail := func(cause error) error { return rb.fail(ctx, cause) }
	rb.cost.observe(result.CostUSD)
	if result.SessionID != "" {
		rb.mu.Lock()
		rb.sessionID = result.SessionID
		rb.mu.Unlock()
	}
	if killedNote != "" {
		rb.report(ctx, killedNote)
	}
	branch, label := rb.branch, rb.label
	switch {
	case errors.Is(runErr, context.DeadlineExceeded):
		return fail(fmt.Errorf("the rebase session ran out of time (%s)", r.Timeout))
	case runErr != nil:
		return fail(fmt.Errorf("rebase session: %w", acct.RedactErr(runErr)))
	}
	result.Text = acct.Redact(result.Text)
	out, ok := harness.ParseRebase(result.Text)
	if !ok {
		return fail(fmt.Errorf("the rebase session reached no result (%s): %s", result.SessionID, truncate(result.Text, 1000)))
	}
	if !out.Rebased {
		return fail(errors.New(out.Reason))
	}
	// what origin has is the head, not what the session says it pushed
	head, err := r.WS.PushedHead(ctx, rb.dir, branch, rb.base)
	if err != nil {
		return fail(fmt.Errorf("the rebase session said it pushed %s, but: %w", short(out.Head), err))
	}
	if !strings.HasPrefix(strings.ToLower(head), out.Head) {
		return fail(fmt.Errorf("the rebase session said it pushed %s, but origin/%s is at %s: the push did not land", short(out.Head), branch, short(head)))
	}
	rb.report(ctx, fmt.Sprintf("rebased %s onto %s at %s", branch, rb.base, short(head)))
	// the rebased head is a commit no attestation names: the platform
	// carries none forward (runsheet ADR 032, The honesty check, rule 1)
	r.validateRebased(ctx, rb, acct, head)
	if err := acct.Carried(r.GW.FinishWorkItemRebase(ctx, rb.item.ID, head, out.Files, acct.Report(result))); err != nil {
		// a finish the platform did not take still ends the claim
		return fail(fmt.Errorf("the rebase landed at %s, but finishing it failed: %w", short(head), err))
	}
	r.logf("item %s: rebased %s@%s, resolved %d file(s) ($%.2f)", label, branch, short(head), len(out.Files), result.CostUSD)
	return nil
}

// AdoptRebase takes up a rebase session a node process before this one
// started (ADR 009): the item's token back on the checkout, the session
// followed to its end — or read from its file when it is gone — with the
// rebase's heartbeat, and then afterRebase. A rebase no longer claimed
// by this node is not its to finish: the session is stopped and its
// directory removed.
func (r *Rebaser) AdoptRebase(ctx context.Context, item gateway.WorkItem, s FoundSession) error {
	ctx = actorContext(ctx, r.Cfg, gateway.RoleBuilder)
	if item.RebaseState != "claimed" || item.RebaseNodeID != "" && item.RebaseNodeID != s.State.NodeID {
		r.logf("item %s: adopted rebase is no longer this node's (%s); stopped", item.ID, item.RebaseState)
		stopFound(r.logf, "item "+item.ID, s)
		return harness.RemoveSession(s.State.Workspace)
	}
	rb := &rebaseRun{r: r, item: item, label: item.ID, dir: s.State.Workspace, sessionID: s.State.SessionID}
	res, err := Resolve(ctx, r.GW, item)
	if err != nil {
		return rb.fail(ctx, fmt.Errorf("resolve: %w", err))
	}
	rb.label = fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	rb.branch = item.Branch
	if rb.branch == "" {
		rb.branch = res.Branch()
	}
	rb.base = res.BaseRef()
	spec := WorkspaceSpec{Root: r.WorkRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: rb.branch, BaseRef: rb.base}
	if rb.cred, err = NewItemCred(ctx, r.GW, r.WS, r.Scopes, item, spec, r.Fallback, r.logf); err != nil {
		return rb.fail(ctx, err)
	}
	at, ok := r.Harness.(harness.Attacher)
	if !ok {
		stopFound(r.logf, "item "+item.ID, s)
		return rb.fail(ctx, errNoAttach)
	}
	r.logf("item %s: adopted its %s rebase session (pid %d)", rb.label, s.Liveness(), s.State.PID)
	result, runErr := rb.run(ctx, func(onProgress func(harness.Progress)) (harness.Result, error) {
		return at.Attach(ctx, s.Dir, onProgress)
	})
	return r.afterRebase(ctx, rb, nil, result, runErr)
}

// contextBudget is the context budget for r's sessions: live, through
// r.ContextBudget, when set; else Cfg's own.
func (r *Rebaser) contextBudget() ContextConfig {
	if r.ContextBudget != nil {
		return r.ContextBudget()
	}
	if r.Cfg == nil {
		return ContextConfig{}.Effective()
	}
	return r.Cfg.Context.Effective()
}

func (r *Rebaser) validator() validator {
	return validator{GW: r.GW, WS: r.WS, Scopes: r.Scopes, Sandbox: r.Sandbox, Inspect: r.Inspect, Notices: r.Notices, logf: r.logf}
}

// validateRebased runs the checkout's declared validation on the head
// the rebase pushed — the steps the branch's diff against its base
// selects plus the always ones, on the host or in the session's pod as a
// build's — and uploads that head's attestation on the node's own token
// (docs/adr/adr-014, After a rebase). Nothing here fails the rebase: its
// job is the head. A failing step is logged and its run still uploaded,
// for the platform to refuse or record; an upload refused or not taken
// is a note or the once-per-run notice, as a build's. A repository with
// no validate.yml says so and finishes as before.
func (r *Rebaser) validateRebased(ctx context.Context, rb *rebaseRun, acct *SessionAccount, head string) {
	vr := r.validator()
	t := validationTarget{item: rb.item, dir: rb.dir, base: rb.base, prefix: "rebase " + rb.label, label: rb.label + " rebase",
		job: rb.job, pod: rb.podSession(), session: rb.sid, redact: acct.RedactErr, report: rb.report}
	note := func(err error) {
		msg := "validation: " + acct.RedactErr(err).Error() + "; no attestation for " + short(head)
		r.logf("%s: %s", t.prefix, msg)
		rb.report(ctx, msg)
	}
	v, err := vr.start(ctx, t)
	if err != nil {
		note(err)
		return
	}
	if v == nil {
		return
	}
	defer v.close()
	if err := checkoutHead(ctx, rb.dir, head); err != nil {
		note(err)
		return
	}
	att, err := vr.run(ctx, t, v)
	if err != nil {
		note(err)
		return
	}
	if failed, ok := failedStep(att); ok {
		rb.report(ctx, fmt.Sprintf("validation step %s exited %d on %s; the run is uploaded for the platform to judge", failed.Name, failed.ExitCode, short(head)))
	}
	vr.upload(ctx, t, att)
}

// checkoutHead leaves the checkout at head: the rebase session pushed
// from it, so HEAD is the pushed head already, and anything else is
// checked out detached — the validation runs on what origin has.
func checkoutHead(ctx context.Context, dir, head string) error {
	at, err := gitHead(ctx, dir)
	if err != nil {
		return err
	}
	if at == head {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "checkout", "--quiet", "--detach", head)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("checkout %s: %v: %s", short(head), err, strings.TrimSpace(string(out)))
	}
	return nil
}
