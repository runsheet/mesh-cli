package node

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
	"projectmesh.io/mesh-cli/internal/tripline"
)

// Evaluator is the second fresh session an item gets on the node
// (mesh-specs trd §3.4): the finished branch checked out, its diff from
// the base, the acceptance, an adversarial review — and its verdict
// recorded with EvaluateWorkItem. The platform opens the pull request on
// a pass and readies the item again on a fail (with the notes for the
// next builder), so the evaluator changes nothing itself. An item
// finished already_done (mesh ADR 003 decision 1) is checked on the base
// ref instead — its evidence and acceptance, no branch and no diff — and
// the same EvaluateWorkItem confirms it (the platform marks it merged,
// with no pull request) or readies it again.
type Evaluator struct {
	GW      Gateway
	Harness harness.Harness
	WS      Workspacer
	Cfg     *Config
	Scopes  Scopes
	// Roots, when set, are each served org's own roots (ADR-034): the
	// item's checkout, clone and scratch go under its org's, WorkRoot
	// unused for them. Nil keeps WorkRoot for every org.
	Roots    *OrgRootSet
	WorkRoot string
	// BuildCache is a host session's GOCACHE; "" is BuildCacheDir().
	BuildCache string
	// MCP are the MCP servers the session gets (runsheet).
	MCP map[string]any
	// Proxies, when set, start each session's sessionproxy: every MCP
	// server reached through one socket, no credential in the session
	// (SessionTools.UseVia).
	Proxies *SessionProxies
	// MaxBudgetUSD caps the review session (3 by default).
	MaxBudgetUSD float64
	// Budgets is node.yaml's `budgets:` in force, read live (ADR 012,
	// Loop.KindModelBudget); nil falls back to Cfg's own, read once at
	// start.
	Budgets func(kind, model string) (amount float64, source string)
	// ContextBudget is node.yaml's `context:` budget in force, read
	// live (Loop.ContextBudget); nil is Cfg's own, read once at start.
	ContextBudget func() ContextConfig
	// Model is the node's own evaluate model (node.yaml evaluate, else
	// default; "" the CLI's default). An item that names a model or an
	// agent with one evaluates on the strongest of this, its build model
	// and sonnet (EvaluateModel); one that names none, on this alone.
	Model string
	// Credentials asks the platform for the session's credentials and
	// reports its usage record. Required: a nil one is a programming
	// error, and its Open panics (no node.yaml fallback, adr-005).
	Credentials *Credentials
	// Timeout caps its wall clock (0 none).
	Timeout time.Duration
	// Fallback is the node's one old-platform notice (nil: every item).
	Fallback *TokenFallback
	// Codex is the Codex harness (nil: none). An evaluation runs on it
	// when one of CodexAccounts has headroom for the item's priority, or
	// the item pins it; and an item secondOpinionWanted picks is evaluated
	// again on the other family, with the same prompt in the same workspace
	// (docs/adr/adr-005-codex-second-opinion.md,
	// docs/adr/adr-011-evaluations-run-on-the-family-with-headroom.md).
	Codex harness.Harness
	// CodexPreflight checks, before any Codex session starts, that the
	// session's job reaches the built-in context server (the Codex
	// harness's Preflight; nil: no check). On a sandboxed node the job
	// carries the evaluation's pod (job.Session), opened before it, so
	// the preflight runs in the pod the session then runs in, with its
	// HOME and env (ADR 007, ADR 011). A failure moves an unpinned
	// evaluation to Claude, keeps a pinned one waiting in evaluating, and
	// makes a Codex second opinion unavailable.
	CodexPreflight func(ctx context.Context, job harness.Job) error
	// CodexModel maps the Claude evaluate model to Codex's
	// (CodexConfig.CodexModel; nil: Codex's own default).
	CodexModel func(claudeModel string) string
	// CodexAccounts are the Codex accounts an evaluation may run on
	// (Loop.CodexCandidates: the issued ones Known holds, and only
	// those). Nil or none: no evaluation and no second opinion runs on
	// Codex — the operator's own Codex login is never one (ADR 011's
	// amendment 2026-10-02).
	CodexAccounts func() []AccountConfig
	// Waits says once per item that an item pinned to a family with no
	// headroom waits in evaluating (nil: every time).
	Waits *OncePer
	// Blind says once per item that an evaluation which saw no context
	// (toolBroken) could not run on the other family either, and the item
	// waits in evaluating (nil: every time).
	Blind *OncePer
	// NoVerdict says once per run that the platform takes no no-verdict
	// outcome, so an evaluation that reached none waits in evaluating
	// (nil: every time).
	NoVerdict *OncePer
	// Splits says once per item that the platform refused the split an
	// item's second failed evaluation asked for (askSplit; nil: every
	// time).
	Splits *OncePer
	// LeftRunning holds, per item, what an evaluation that recorded no
	// verdict left running, for the notes of the item's next verdict
	// (nil: only the session's own).
	LeftRunning *LeftRunning
	// Limits and Thresholds judge the Codex accounts (nil: never
	// skipped, every account with its full headroom).
	Limits     *Limits
	Thresholds func() LimitsConfig
	// Runtimes are the tool runtimes the node has now (Loop.Runtimes;
	// nil: none), which a local tool needs to start.
	Runtimes func() []string
	// Declines remembers an item not evaluated for a required tool whose
	// runtime the node lacks, so the loop does not try it again (nil:
	// not remembered).
	Declines *Declines
	// Tripline reports the node's failure classes to Tripline (ADR-034,
	// a node reports its errors to Tripline); nil reports nothing.
	Tripline *tripline.Reporter
	// RuntimeBroken is told the error when a session's pod could not
	// start on a broken container runtime (a *sandbox.RuntimeError) and
	// its work was handed back runtime_broken; nil tells no one.
	RuntimeBroken func(error)
	// Sandbox, when set, runs the session in its own pod with the
	// services its checkout declares (as Runner.Sandbox).
	Sandbox *sandbox.Sandbox
	// HeartbeatEvery is how often an evaluation heartbeats its item,
	// claim to verdict (evalHeartbeat); 0 is node.yaml's
	// evaluate.heartbeat_every, else DefaultEvaluationHeartbeatEvery.
	HeartbeatEvery time.Duration
	Log            *log.Logger
}

// kindModelBudget is node.yaml's budgets: figure for kind and model —
// live, through e.Budgets, when set; else Cfg's own, read once at start.
func (e *Evaluator) kindModelBudget(kind, model string) (float64, string) {
	if e.Budgets != nil {
		return e.Budgets(kind, model)
	}
	return e.Cfg.KindModelBudget(kind, model)
}

// budgetWithSource is the evaluation's cost cap (ADR 012) and where it
// came from, for the session's starting log line.
func (e *Evaluator) budgetWithSource(goal *gateway.Goal, model string) (float64, string) {
	kindBudget, kindSource := e.kindModelBudget("evaluate", model)
	fallback := e.MaxBudgetUSD
	if fallback <= 0 {
		fallback = 3
	}
	return SessionBudget(kindBudget, kindSource, goal, fallback)
}

func (e *Evaluator) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log.Printf(format, args...)
	}
}

func (e *Evaluator) runtimes() []string {
	if e.Runtimes == nil {
		return nil
	}
	return e.Runtimes()
}

// goneBaseFallback is def, and true, when err says base — a merged
// dependency's branch — is gone from origin (*BaseGoneError) and def,
// the repo's default branch, is another branch to diff against. Any
// other error, or none, is false: failDiff still records it.
func goneBaseFallback(err error, base, def string) (string, bool) {
	var gone *BaseGoneError
	if err == nil || !errors.As(err, &gone) || def == "" || base == def {
		return "", false
	}
	return def, true
}

// changedPathsFrom is ws's changed paths in dir against base, else,
// base gone from origin, against def; and the branch they were taken
// from.
func changedPathsFrom(ctx context.Context, ws Workspacer, dir, base, def string) ([]string, string, error) {
	paths, err := ws.ChangedPaths(ctx, dir, base)
	if fb, ok := goneBaseFallback(err, base, def); ok {
		paths, err = ws.ChangedPaths(ctx, dir, fb)
		return paths, fb, err
	}
	return paths, base, err
}

// failDiff records a diff the node cannot compute as the item's failed
// evaluation, with the reason for its next builder: no session starts and
// no account is charged. Retrying would fail the same way forever.
func (e *Evaluator) failDiff(ctx context.Context, item gateway.WorkItem, label, base, branch string, diffErr error) error {
	var notes string
	if nm := (*NoMergeBaseError)(nil); errors.As(diffErr, &nm) {
		notes = fmt.Sprintf("The branch %s shares no history with origin/%s: it was rebuilt from scratch or from an unrelated root, so there is nothing to diff or review. Rebuild it on origin/%s (start from the base, not a new root).", branch, base, base)
	} else {
		notes = fmt.Sprintf("the evaluation could not diff the branch against origin/%s: %v", base, diffErr)
	}
	notes = truncate(notes, 4000)
	e.logf("item %s: not evaluated, failed: %s", label, notes)
	stopHeartbeat(ctx)
	if err := e.GW.EvaluateWorkItem(ctx, item.ID, false, notes, nil, 0, nil, gateway.EvaluateChoice{}); err != nil {
		return fmt.Errorf("record verdict: %w", err)
	}
	return nil
}

// evaluationNotYours reports whether err is issueItemRepoToken or
// evaluateWorkItem refused because another node already holds this
// item's evaluation (ADR-009's "you are not the node evaluating the
// item") — a gateway refusal naming "not yours", read the same way
// owned.go's NotYoursLine reads a registration's.
func evaluationNotYours(err error) bool {
	return gateway.IsRefusal(err) && strings.Contains(err.Error(), "not yours")
}

// Evaluate reviews one item in evaluating and records the verdict. An
// error means no verdict was recorded: the item stays in evaluating and
// the next tick tries again. A diff the node cannot compute is the
// exception: it is recorded as a failed evaluation with the reason (no
// session, no cost) and Evaluate returns nil, so it is never retried
// without end. That holds when GitHub refused the item's
// token through every retry too: the item is neither failed nor released
// (the platform releases only a claimed or running item), the node logs
// the credential reason and a later tick evaluates it on a fresh token.
func (e *Evaluator) Evaluate(ctx context.Context, item gateway.WorkItem) error {
	// every command the evaluation sends acts as its evaluator (ADR-015)
	ctx = actorContext(ctx, e.Cfg, gateway.RoleEvaluator)
	res, err := Resolve(ctx, e.GW, item)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	ctx = WithLockHolder(ctx, label)
	// the node holds the evaluation from here until its verdict: the
	// item heartbeats all the while, whatever the session says, so a
	// long suite is never stalled (runsheet ADR 018's amendment
	// 2026-10-18)
	ctx, hb := e.startHeartbeat(ctx, item.ID, label, evalPhaseCheckout)
	defer hb.end()
	return e.claimLost(hb, label, e.evaluate(ctx, item, res, label))
}

// evaluate is Evaluate's work once the item is resolved and its
// heartbeat runs on ctx.
func (e *Evaluator) evaluate(ctx context.Context, item gateway.WorkItem, res *Resolved, label string) error {
	// the family it runs on, before anything is prepared: a pinned
	// family with no headroom waits in evaluating (ADR 011)
	fam, err := e.family(ctx, item, res.Goal.Priority, label)
	if err != nil {
		return err
	}
	// an already_done item has no branch: the evaluator checks its
	// evidence on the base ref, with its history, and there is no diff
	// (mesh ADR 003 decision 1)
	alreadyDone := item.Outcome == gateway.OutcomeAlreadyDone
	branch := item.Branch
	if alreadyDone {
		branch = ""
	} else if branch == "" {
		branch = res.Branch()
	}
	// a checkout of its own, never the build's: its Authorize must not
	// rewrite the credential helper the build's push reads
	root, err := workRootFor(e.Roots, e.Scopes, item.OrgID, e.WorkRoot)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	spec := EvalWorkspaceSpec(root, res.Goal.Slug, item.Seq, res.Repo.FullName(), branch, res.BaseRef())
	// live on this node while this runs: no cleanup touches it (HoldWorkspace)
	defer HoldWorkspace(spec.Dir())()
	// the item's own GitHub token, as its builder had
	cred, err := NewItemCred(ctx, e.GW, e.WS, e.Scopes, item, spec, e.Fallback, e.logf)
	if we := WorkspaceErrorOf(err); we != nil {
		return e.releaseInfrastructure(ctx, item, label, we)
	}
	if evaluationNotYours(err) {
		// another node already converged on this item's evaluation
		// (ADR-009): no session, no verdict, no release — it is left to
		// that node, quietly, as a refusal of this ordinary kind always is
		e.logf("item %s: evaluation is another node's (%v); left to it", label, err)
		return nil
	}
	if err != nil {
		return err
	}
	dir, err := PrepareWithRetry(ctx, e.WS, spec, cred, e.logf)
	if ce := (*CredentialError)(nil); errors.As(err, &ce) {
		e.logf("item %s: not evaluated, the node's condition: %s", label, ce.Reason())
		return fmt.Errorf("workspace: %w", ce)
	}
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	var diff string
	// diffBase is what the branch is diffed and judged against: the item's
	// base, else, a merged dependency's branch gone from origin, the
	// repo's default branch, which then holds the dependency
	diffBase := spec.BaseRef
	if !alreadyDone {
		diff, err = e.WS.Diff(ctx, dir, diffBase)
		if def, ok := goneBaseFallback(err, diffBase, defaultBranchOf(res)); ok {
			e.logf("item %s: evaluate: base %s is gone (merged); diff from origin/%s", label, diffBase, def)
			diffBase = def
			diff, err = e.WS.Diff(ctx, dir, diffBase)
		}
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("diff: %w", err)
			}
			return e.failDiff(ctx, item, label, diffBase, branch, err)
		}
	}
	changed, cfg := e.touched(ctx, label, dir, diffBase, defaultBranchOf(res), alreadyDone)
	// the same context the builder had: its repo's instructions and its goal
	orgName := e.Scopes.NameOf(item.OrgID)
	cx, err := FetchContext(ctx, e.GW, orgName, res.Project.Slug, ContextOptions{Budget: e.contextBudget(), Repos: []string{res.Repo.FullName()}, Checkouts: map[string]string{res.Repo.FullName(): dir}, Goal: res.Goal, Logf: e.logf})
	if err != nil {
		e.logf("item %s: context unavailable: %v", label, err)
		cx = &Context{}
	}
	files, written, err := WriteContext(dir, cx)
	if err != nil {
		return fmt.Errorf("write context: %w", err)
	}
	e.logf("item %s: %s", label, written.Line())
	if w := written.TargetWarning(); w != "" {
		e.logf("item %s: %s", label, w)
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 || cx.Live != nil {
		contextDir = ContextDir
	}
	prior := e.priorAttempts(ctx, item, label)
	in := harness.EvaluateInput{
		ItemID: item.ID, GoalSlug: res.Goal.Slug, GoalTitle: res.Goal.Title, GoalAcceptance: res.Goal.Acceptance,
		Seq: item.Seq, Title: item.Title, Brief: item.Brief, Acceptance: item.Acceptance,
		Repo: res.Repo.FullName(), Branch: branch, BaseRef: diffBase, Diff: diff,
		ContextDir: contextDir, ContextFiles: files, Project: orgName + "/" + res.Project.Slug, MeshContext: len(e.MCP) > 0,
		AlreadyDone: alreadyDone, Evidence: item.Evidence,
		PriorAttempts: prior,
	}
	model := fam.model
	budget, budgetSource := e.budgetWithSource(res.Goal, model)
	e.logf("item %s: evaluate budget $%.2f (%s)", label, budget, budgetSource)
	kept, err := keptRootFor(e.Roots, e.Scopes, item.OrgID)
	if err != nil {
		return err
	}
	job := harness.Job{Kind: harness.KindEvaluate, Workspace: dir, Prompt: harness.EvaluatePrompt(in), MaxBudgetUSD: budget, Label: label + " evaluate", Timeout: e.Timeout, MCPServers: e.MCP, Model: model, KeptRoot: kept}
	if tok := cred.Token(); tok != nil {
		ghDir, err := NewGHConfigDir()
		if err != nil {
			return fmt.Errorf("gh config dir: %w", err)
		}
		// a session left running for the next node keeps its dirs: the
		// node that ends it removes them (TempDirs)
		defer unlessDetached(ctx, func() { os.RemoveAll(ghDir) })
		job.Env = SessionEnv(tok, ghDir)
	}
	// the session's own scratch as its TMPDIR, gone with the session
	// (sessionScratch); a detached one's by the node that ends it
	scratchRoot, err := scratchRootFor(e.Roots, e.Scopes, item.OrgID, e.WorkRoot)
	if err != nil {
		return err
	}
	scratch, err := sessionScratch(e.Sandbox, scratchRoot, e.BuildCache, harness.KindEvaluate, item.ID, &job)
	if err != nil {
		return err
	}
	defer unlessDetached(ctx, func() { RemoveScratch(scratch) })
	sc, _ := e.Scopes.ByOrg(item.OrgID)
	ref := gateway.SessionRef{Kind: string(harness.KindEvaluate), ItemID: item.ID, GoalID: item.GoalID}
	ctx = withSubject(ctx, sc, cmp.Or(item.ProjectID, res.Project.ID), res.Project.Slug)
	acct, fam, err := e.openFamily(ctx, ref, sc.NodeID, fam, label)
	if err != nil {
		return err
	}
	// whichever account the session ends on (a failed preflight swaps it)
	defer unlessDetached(ctx, func() { acct.Cleanup() })
	runtimes := e.runtimes()
	// the checkout's services, started in the session's pod before the
	// evaluator — and before a Codex preflight, which runs in this pod
	// so it proves what the session will see (ADR 007, ADR 011); the pod
	// and its sidecars go on every way out. An item
	// in evaluating can be neither released nor reported on: a required
	// service that does not come up leaves it unevaluated, and the loop
	// does not try again while that holds; a missing optional one is in
	// the verdict's notes.
	pod, err := openOrgSessionPod(ctx, e.Sandbox, e.Roots, e.Scopes, item.OrgID, dir, job.Label, harness.KindEvaluate, sandbox.GitIdentity{}, e.logf)
	defer pod.Close() // nothing, once the harness detached the session (Session.Detach)
	if re := RuntimeErrorOf(err); re != nil {
		return e.runtimeNoVerdict(ctx, evalRun{item: item, res: res, label: label, model: model, fam: fam, dir: dir, acct: acct}, re, harness.Result{})
	}
	if err != nil {
		return fmt.Errorf("services: %w", err)
	}
	if _, failed := pod.RequiredFailure(); failed {
		reason := pod.ReleaseReason()
		e.Declines.Record(item.ID, pod.Declined(), runtimes, time.Now())
		e.logf("item %s: not evaluated, %s", label, reason)
		return fmt.Errorf("not evaluated, %s", reason)
	}
	if note := pod.MissingNote(); note != "" {
		e.logf("item %s: %s", label, note)
	}
	base := job
	base.Env = slices.Clone(job.Env)
	var second harness.Job
	var st SessionTools
	preflight := ""
	for {
		job = base
		job.Env = slices.Clone(base.Env)
		model, job.Model = fam.model, fam.model
		job.Meta = harness.SessionMeta{NodeID: sc.NodeID, ItemID: item.ID, GoalID: item.GoalID, Attempt: item.Attempts, TempDirs: sessionTempDirs(job, acct), LoginCache: acct.loginCache()}
		// the second opinion's job: the same brief, none of the first
		// session's account and none of the pod's services (Codex runs on
		// the host; a Claude second opinion opens its own account)
		second = job
		second.Env = slices.Clone(job.Env)
		acct.Apply(&job)
		// the evaluator's tools, beside the built-in runsheet. An item in
		// evaluating cannot be released (the platform releases a claimed or
		// running one): a required tool this node cannot start leaves it
		// unevaluated, and the loop does not try again while that holds.
		st = SelectTools(e.MCP, acct.Tools, tools.RoleFor(string(harness.KindEvaluate)), runtimes)
		if len(st.Missing) > 0 {
			e.Declines.Record(item.ID, st.Missing, runtimes, time.Now())
			return fmt.Errorf("declined, %s", st.DeclineReason())
		}
		closeProxy, err := st.UseVia(e.Proxies, mcpRootFor(e.Roots, e.Scopes, item.OrgID), &job, e.MCP, acct)
		if err != nil {
			return fmt.Errorf("tools: %w", err)
		}
		defer closeProxy(ctx)
		if !fam.codex {
			break
		}
		// a Codex session that cannot reach the context server would judge
		// blind: it does not start, and a tool failure never becomes a
		// verdict (ADR 011, the preflight)
		pf := job
		pod.Apply(&pf)
		why := e.preflight(ctx, pf, acct)
		if why == "" {
			e.Waits.Forget(preflightKey(item.ID))
			break
		}
		acct.Cleanup()
		if fam.pinned {
			return e.preflightWaits(item.ID, label, fam.model, why)
		}
		e.logf("item %s: Codex preflight failed (%s); evaluating on Claude", label, why)
		preflight = why
		fam = family{model: fam.claude, claude: fam.claude, priority: fam.priority}
		// never overwrite acct with Open's nil: the deferred Cleanup
		// above reads it, and a nil there panicked the node (the
		// 04:00:22Z exit, goal node-yaml-takes-effect-or-says-why-not)
		claudeAcct, err := e.Credentials.Open(ctx, ref, sc.NodeID, AdapterClaudeCode)
		if err != nil {
			return err
		}
		acct = claudeAcct
	}
	acct.SetContextBytes(written.Bytes)
	acct.SetLabel("item "+label, string(harness.KindEvaluate))
	if len(st.Use) > 0 {
		e.logf("item %s: evaluate with tools %s", label, st.Names())
	}
	if note := st.OmittedNote(); note != "" {
		e.logf("item %s: %s", label, note)
	}
	pod.Apply(&job)
	in.Services = pod.Prompt()
	job.Prompt = harness.EvaluatePrompt(in)
	if alreadyDone {
		e.logf("item %s: evaluate on %s: the already_done claim on %s", label, fam.logName(), spec.BaseRef)
	} else {
		e.logf("item %s: evaluate on %s: %s (%d bytes of diff)", label, fam.logName(), branch, len(diff))
	}
	setEvalPhase(ctx, evalPhaseSession)
	h := e.Harness
	// the session outlives this process: a restart adopts it (ADR 009) —
	// a Codex one in its pod on a sandboxed node, by its state's adapter
	// (AdoptEvaluation); Codex on the host still ends with the node
	job.Outlive = !fam.codex || e.Sandbox != nil
	if fam.codex {
		h = e.Codex
	}
	result, err := h.Run(ctx, job, nil)
	// the usage report rides the verdict (afterEvaluate); Finish logs it
	// when nothing carried it
	defer acct.Finish(ctx, result)
	return e.afterEvaluate(ctx, evalRun{item: item, res: res, label: label, model: model, fam: fam, dir: dir, acct: acct, second: &second, preflight: preflight, omitted: st.OmittedNote(), missing: pod.MissingNote(), prior: prior, changed: changed, setting: cfg.setting(), protected: cfg.ProtectedPaths}, result, err)
}

// priorAttempts reads the verdicts earlier attempts of the item were
// given. A retry is judged on what earlier attempts failed on, so an
// item that has answered every objection already named is not failed on
// a fresh reading of the acceptance (evaluateRetryRule), and a verdict
// failing a clause one of them failed opens by saying so (ADR-020).
// Unread records are not fatal: the evaluation runs without them.
func (e *Evaluator) priorAttempts(ctx context.Context, item gateway.WorkItem, label string) []harness.PriorAttempt {
	if item.Attempts == 0 {
		return nil
	}
	was, err := e.GW.WorkItemPriorAttempts(ctx, item.ID)
	if err != nil {
		e.logf("item %s: earlier evaluations unread, evaluating without them: %v", label, err)
	}
	var prior []harness.PriorAttempt
	for _, p := range was {
		at := harness.PriorAttempt{Attempt: p.Attempt, Notes: p.Notes}
		for _, c := range p.Clauses {
			at.Clauses = append(at.Clauses, harness.FailedClause{Clause: c.Clause, Satisfy: c.Satisfy})
		}
		prior = append(prior, at)
	}
	return prior
}

// evalRun is one evaluation session as its tail needs it, whether this
// process started it (Evaluate) or adopted it (AdoptEvaluation).
type evalRun struct {
	item  gateway.WorkItem
	res   *Resolved
	label string
	model string
	// fam is the family the session ran on (Claude for an adopted one)
	fam  family
	dir  string
	acct *SessionAccount // nil for an adopted session
	// second is the second opinion's job; nil for an adopted session,
	// whose prompt is not kept
	second *harness.Job
	// preflight is why a Codex preflight failed and the session runs on
	// Claude instead ("" when none did)
	preflight        string
	omitted, missing string
	// adopted is a session an older node process started and this one
	// took up (AdoptEvaluation): only there may the built-in carry its old name
	adopted bool
	// prior are the verdicts earlier attempts were given, for the
	// same-clause first line (ADR-020)
	prior []harness.PriorAttempt
	// changed are the files the evaluated diff changed, setting the
	// checkout's second_opinion and protected its protected_paths, for
	// secondOpinionWanted
	changed   []string
	setting   string
	protected []string
}

// touched is what the evaluation's choice of evaluators reads from the
// checkout: the files the branch changed since base (none for an
// already_done item, which has no diff) and its .mesh/config.yml
// second_opinion and protected_paths. A base gone from origin is read
// against def, the repo's default branch. Unread changed paths are logged and read as none: the
// item's own protectedFiles still hold.
func (e *Evaluator) touched(ctx context.Context, label, dir, base, def string, alreadyDone bool) ([]string, checkoutConfig) {
	var changed []string
	if !alreadyDone && e.WS != nil {
		var err error
		if changed, _, err = changedPathsFrom(ctx, e.WS, dir, base, def); err != nil {
			e.logf("item %s: changed paths unread, judging protected paths on protectedFiles alone: %v", label, err)
		}
	}
	return changed, readCheckoutConfig(dir)
}

// afterEvaluate is what follows an evaluation session, however this
// process came to hold it: the verdict read, the second opinion asked
// when secondOpinionWanted says so, and the verdict recorded. An error
// means none was recorded. A cancelled ctx leaves the session running
// for the next node to adopt.
func (e *Evaluator) afterEvaluate(ctx context.Context, ev evalRun, result harness.Result, err error) error {
	item, label, acct := ev.item, ev.label, ev.acct
	// what the agent left running goes in the verdict's notes; an end
	// that records no verdict only logs it and holds it for the item's
	// next one, since a progress note is the evaluation's heartbeat,
	// not its record (ADR 008, The session's end). Logged before a done ctx
	// returns: a deadline kills the unit too, and a detach kills nothing.
	left := e.LeftRunning
	if left == nil {
		left = &LeftRunning{}
	}
	killed := left.Hold(item.ID, logKilled(e.logf, "item "+label, result.KilledPerSession(), result.KillError))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	defer harness.RemoveSession(ev.dir)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("evaluator session ran out of time (%s)", e.Timeout)
	}
	if re := RuntimeErrorOf(err); re != nil {
		return e.runtimeNoVerdict(ctx, ev, re, result)
	}
	if err != nil {
		return fmt.Errorf("evaluator session: %w%s", acct.RedactErr(err), keptAt(result))
	}
	result.Text = acct.Redact(result.Text)
	if ev.fam.codex && acct != nil {
		acct.Model = ev.fam.model
	}
	usage := acct.Report(result)
	// fam is the family whose verdict stands: the session's, or the other
	// one's when the session saw no context (ADR 011, a tool failure is
	// never a verdict); head then names the attempt that gave none
	fam, head, blindOn := ev.fam, "", ""
	var passed bool
	var notes string
	// the clauses the deciding session failed, for the retry to fix
	var clauses []harness.FailedClause
	// verdictCalled: the deciding session ended by a mesh_verdict call
	// (ADR-023), which the hosted tool may have recorded already
	var verdictCalled bool
	cost := result.CostUSD
	if blind, what, broken := e.unreadable(result, acct, ev.adopted); broken {
		other := ev.fam.other(e)
		saw := fmt.Sprintf("evaluation on %s %s; not a verdict", ev.fam.fullName(), what)
		if why, ok := e.canRetry(ev, other); !ok {
			reason := fmt.Sprintf("%s, and %s cannot evaluate it: %s", saw, other.name(), why)
			return e.noVerdict(ctx, ev, reason, cost, usage, func() error { return e.blind(item.ID, "item "+label+": "+reason) })
		}
		e.logf("item %s: %s, evaluating on %s", label, saw, other.fullName())
		op := e.secondOpinion(ctx, ev, other, *ev.second, "evaluation")
		defer op.acct.Finish(ctx, op.result)
		killed = left.Hold(item.ID, op.killed)
		// one report for the whole evaluation, the attempt with no verdict
		// in it too
		if op.acct != nil {
			acct.Carries(op.acct)
		} else {
			acct.Add(op.result)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if op.unavailable != "" {
			reason := fmt.Sprintf("%s, and the evaluation on %s is unavailable too: %s", saw, other.fullName(), op.unavailable)
			return e.noVerdict(ctx, ev, reason, cost+op.cost, usage, func() error { return e.blind(item.ID, "item "+label+": "+reason) })
		}
		e.Blind.Forget(item.ID)
		head = fmt.Sprintf("%s (first attempt, %s): no verdict — %s", ev.fam.name(), ev.fam.shortModel(), blind)
		blindOn = blind
		fam, passed, notes, clauses, verdictCalled = other, op.passed, op.notes, op.clauses, op.called
		cost += op.cost
	} else {
		var ok bool
		passed, notes, clauses, verdictCalled, ok = verdictOf(result, acct.Redact)
		if !verdictCalled {
			e.logf("item %s: %s", label, noVerdictCallLine)
		}
		if !ok {
			return e.noVerdict(ctx, ev, "no VERDICT line: "+truncate(result.Text, 300), cost, usage, func() error {
				return fmt.Errorf("evaluator session reached no verdict (%s): %s%s", result.SessionID, truncate(result.Text, 300), keptAt(result))
			})
		}
		notes = strings.TrimSpace(notes)
		if notes == "" {
			notes = "(no notes)"
		}
	}
	// a host reading is recorded, not failed: it rides the notes, and
	// node.log says so (ADR-020)
	e.logReadings(label, notes)
	// the notes say which tools and services the evaluator ran without
	without := ""
	if ev.omitted != "" {
		without = "\n" + truncate(ev.omitted, 500)
	}
	if ev.missing != "" {
		without += "\n" + truncate(ev.missing, 500)
	}
	// and which served tools it never called, or called with errors
	if note := acct.UseNote(result); note != "" {
		e.logf("item %s: %s", label, note)
		without += "\n" + truncate(note, 500)
	}
	if result.KeptSessionDir != "" && hasToolError(result) {
		e.logf("item %s: a tool errored%s", label, keptAt(result))
	}
	room := 4000
	if head != "" {
		room -= len(head) + 2
	}
	if len(ev.prior) > 0 {
		// for the same-clause first line a failing retry may open with
		room -= sameClauseRoom
	}
	// the item's events say which family and model judged it
	evaluatedOn := fam.logName()
	if ev.preflight != "" && head == "" {
		// and why not on Codex, which the item might have expected
		evaluatedOn = fam.fullName() + "; the Codex preflight failed: " + truncate(ev.preflight, 200)
	}
	// and what its sessions left running that their ends killed
	if killed != "" {
		without += "\n" + truncate(killed, 500)
	}
	// one evaluator or two, by the rule (goal one-evaluator-for-ordinary-work)
	wanted, why := secondOpinionWanted(evalDecision{
		Priority:       ev.res.Goal.Priority,
		ProtectedFiles: item.ProtectedFiles, ProtectedRulePaths: item.ProtectedRulePaths,
		ProtectedPaths: append(append([]string(nil), projectProtectedPaths(ev.res)...), ev.protected...), Changed: ev.changed,
		FirstPassed: passed, FailedClauses: clauses, Setting: ev.setting,
	})
	firstPassed := passed
	// evaluators is 1 or 2, for the usage record (adr-016's amendment
	// 2026-10-02): 2 only where the notes below gain the "second opinion
	// on" tail, never for a blind retry on the other family, whose
	// verdict stands alone.
	evaluators := 1
	choice := evaluatorsLine(1, why)
	if wanted && !(fam.codex || e.hasCodex()) {
		choice += ", but no other family on this node"
	} else if wanted {
		choice = evaluatorsLine(2, why)
	}
	e.logf("item %s: %s", label, choice)
	room -= len(choice) + 1
	if wanted && (fam.codex || e.hasCodex()) {
		// the second opinion is the other family — which, after a first
		// attempt that saw no context, is the one that just could not reach
		// it: it is not asked again, and the verdict reached stands (ADR
		// 011's amendment 2026-09-29, the pass in the record)
		other := fam.other(e)
		if head != "" {
			other = ev.fam
		}
		op := opinion{unavailable: "the evaluation was adopted after a restart, and its prompt is not kept"}
		switch {
		case blindOn != "":
			op = opinion{unavailable: "it saw no context on the first attempt (" + blindOn + ")"}
		case ev.preflight != "" && other.codex:
			// the preflight that moved it to Claude has just failed
			op = opinion{unavailable: "preflight " + ev.preflight}
		case ev.second != nil:
			op = e.secondOpinion(ctx, ev, other, *ev.second, "second opinion")
		}
		defer op.acct.Finish(ctx, op.result)
		left.Hold(item.ID, op.killed)
		if op.killed != "" {
			without += "\n" + truncate(op.killed, 500)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cost += op.cost
		// one report for the whole evaluation, as its cost is one
		if op.acct != nil {
			acct.Carries(op.acct)
		} else {
			acct.Add(op.result)
		}
		if op.unavailable != "" {
			e.logf("item %s: %s second opinion unavailable: %s", label, other.name(), op.unavailable)
			line := other.name() + " second opinion unavailable: " + op.unavailable
			notes = truncateNotes(notes, room-200-len(line)-len(without)-len(ev.preflight)) + "\n\n" + line + "\n\n" + choice + "\n(evaluated on " + evaluatedOn + ")" + without
		} else {
			e.logReadings(label, op.notes)
			// the second verdict decides: after a first PASS a second fail
			// fails it; after a first FAIL a second fail fails it on the
			// union of both clauses, and a second pass passes it (a wrong
			// fail costs a rebuild)
			switch {
			case op.passed:
				clauses = nil
			case firstPassed:
				clauses = op.clauses
			default:
				clauses = unionClauses(clauses, op.clauses)
			}
			passed = op.passed
			notes = bothNotes(fam, notes, firstPassed, other, op.notes, op.passed, room-200-len(without)-len(ev.preflight)) + "\n\n" + choice + "\n(evaluated on " + evaluatedOn + "; second opinion on " + other.fullName() + ")" + without
			evaluators = 2
		}
	} else {
		notes = truncateNotes(notes, room-100-len(without)-len(ev.preflight)) + "\n\n" + choice + "\n(evaluated on " + evaluatedOn + ")" + without
	}
	if head != "" {
		notes = head + "\n\n" + notes
	}
	verdict := "failed"
	if passed {
		verdict = "passed"
	}
	var failed []gateway.FailedClause
	if !passed {
		for _, c := range clauses {
			failed = append(failed, gateway.FailedClause{Clause: c.Clause, Satisfy: c.Satisfy})
		}
		// a second failure on a clause an earlier attempt failed says so
		// first, so the narrow re-plan changes the clause or escalates
		// rather than rebuilding (ADR-020)
		if attempt, _, ok := harness.SameClause(ev.prior, clauses); ok {
			notes = truncateNotes(harness.NoteSameClause(notes, ev.prior, clauses), 4000)
			e.logf("item %s: fails on the same clause as attempt %d", label, attempt)
		}
	}
	acct.SetEvaluation(ev.res.Goal.Priority, evaluators)
	// no heartbeat after the verdict
	stopHeartbeat(ctx)
	if err := acct.Carried(e.GW.EvaluateWorkItem(ctx, item.ID, passed, notes, failed, Cents(cost), usage, gateway.EvaluateChoice{EvaluatedModel: fam.model})); err != nil {
		if verdictCalled && gateway.IsRefusal(err) && !gateway.IsUnsupported(err) {
			// the hosted mesh_verdict already recorded it, so the item is
			// no longer evaluating (ADR-023)
			left.Forget(item.ID)
			e.logf("item %s: evaluated by tool call on the platform", label)
			return nil
		}
		if !verdictCalled && evaluationNotYours(err) {
			// another node converged on the evaluation while this session
			// ran; its own verdict (or tool call) is the one of record, and
			// retrying this one forever would never land it
			left.Forget(item.ID)
			e.logf("item %s: verdict refused: another node holds the evaluation", label)
			return nil
		}
		return fmt.Errorf("record verdict: %w", err)
	}
	left.Forget(item.ID)
	e.logf("item %s: evaluation %s on %s ($%.2f)", label, verdict, fam.fullName(), cost)
	if !passed {
		e.askSplit(ctx, item, label, ev.prior)
	}
	return nil
}

// splitAt is the failed evaluation after which the evaluator asks for a
// split rather than leave a third build to the platform (ADR-021's
// amendment 2026-10-02). The third failure is the platform's own narrow
// re-plan (runsheet ADR 023, rung 3), so the node asks at this one alone.
const splitAt = 2

// failedVerdicts counts the failed verdicts of an item: the earlier
// attempts' records whose notes carry no `VERDICT: PASS` and are not a
// no-verdict's reason (an unreached evaluation is not a failed one,
// runsheet ADR 035), plus this failing one.
func failedVerdicts(prior []harness.PriorAttempt) int {
	n := 1
	for _, p := range prior {
		if strings.Contains(p.Notes, harness.VerdictPass) || noVerdictRecord(p.Notes) {
			continue
		}
		n++
	}
	return n
}

// noVerdictRecord says whether a recorded evaluation's notes are the
// reason an evaluation reached no verdict, as the evaluator writes it
// (noVerdict): no VERDICT line, or a session whose tools or sandbox
// failed — `…; not a verdict`. The platform keeps those out of
// `evaluations` (runsheet ADR 023's amendment 2026-09-29); this keeps a
// record that slipped in from counting.
func noVerdictRecord(notes string) bool {
	n := strings.TrimSpace(notes)
	return strings.HasPrefix(n, "no VERDICT line:") || strings.Contains(n, "; not a verdict")
}

// askSplit is the evaluator's ask after an item's second failed
// evaluation: a narrow re-plan replacing the item (RequestPlanReplacing),
// which the planner answers with two or more smaller items (planSplitRule)
// instead of a third build at the same size. A refusal is the
// platform's ladder going on, said once per item and never an error; a
// platform without replaceItem is sent nothing.
func (e *Evaluator) askSplit(ctx context.Context, item gateway.WorkItem, label string, prior []harness.PriorAttempt) {
	if failedVerdicts(prior) != splitAt || item.GoalID == "" {
		return
	}
	sc, _ := e.Scopes.ByOrg(item.OrgID)
	err := e.GW.RequestPlanReplacing(ctx, item.GoalID, sc.NodeID, item.ID)
	switch {
	case errors.Is(err, gateway.ErrNoReplaceItem):
		return
	case err == nil:
		e.logf("item %s: failed evaluation %d; asking for a split (narrow re-plan) instead of a third build", label, splitAt)
	case gateway.IsRefusal(err):
		if e.Splits.First(item.ID) {
			e.logf("item %s: split refused by the platform: %v; the platform's ladder continues", label, err)
		}
	default:
		e.logf("item %s: split not asked: %v; the platform's ladder continues", label, err)
	}
}

// sameClauseRoom is what a failing retry's notes leave for the line
// NoteSameClause may open them with.
const sameClauseRoom = 400

// truncateNotes cuts a verdict's notes to n as truncate does, but
// never its READING lines: the review above them is cut and they follow
// it, so a reading node.log says was recorded is in the recorded notes
// (ADR-020).
func truncateNotes(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	var body, readings []string
	for _, line := range strings.Split(s, "\n") {
		if len(harness.ParseReadings(line)) > 0 {
			readings = append(readings, strings.TrimSpace(line))
		} else {
			body = append(body, line)
		}
	}
	if len(readings) == 0 {
		return truncate(s, n)
	}
	keep := strings.Join(readings, "\n")
	room := n - len(keep) - 3 // the newline, and truncate's "…" over the byte it replaces
	if room < 2 {
		return truncate(keep, n)
	}
	return truncate(strings.Join(body, "\n"), room) + "\n" + keep
}

// logReadings logs each READING line of a verdict's notes: the
// evaluator's own reading of a host clause, recorded with the verdict
// rather than failed (ADR-020).
func (e *Evaluator) logReadings(label, notes string) {
	for _, r := range harness.ParseReadings(notes) {
		e.logf("item %s: reading recorded: %s", label, r.Clause)
	}
}

// noVerdict records an evaluation that reached no verdict — the deciding
// session wrote no VERDICT line, or saw no context and the other family
// could not evaluate it — as a no-verdict (`passed: false`, `noVerdict:
// true`, the reason; runsheet ADR 035) with the session's cost and its
// usage report (ADR 011's amendments 2026-09-29 and 2026-10-02): never a
// fail, since the evaluator had no way to reach one. A platform without
// `noVerdict` (gateway.ErrNoVerdictOutcome) records nothing, as
// before: said once per run, and fallback is the error the item waits in
// evaluating with.
func (e *Evaluator) noVerdict(ctx context.Context, ev evalRun, reason string, cost float64, usage *gateway.UsageReport, fallback func() error) error {
	reason = truncate(reason, 4000)
	stopHeartbeat(ctx)
	err := e.GW.EvaluateWorkItemNoVerdict(ctx, ev.item.ID, reason, Cents(cost), usage, gateway.EvaluateChoice{EvaluatedModel: ev.fam.model})
	if errors.Is(err, gateway.ErrNoVerdictOutcome) {
		if e.NoVerdict.First("") {
			e.logf("the platform takes no no-verdict outcome; a no-verdict evaluation waits in evaluating (said once per run)")
		}
		return fallback()
	}
	if err := ev.acct.Carried(err); err != nil {
		return fmt.Errorf("record no verdict: %w", err)
	}
	e.Blind.Forget(ev.item.ID)
	e.logf("item %s: evaluation reached no verdict on %s, recorded as no verdict ($%.2f): %s", ev.label, ev.fam.fullName(), cost, reason)
	return nil
}

// AdoptEvaluation takes up an evaluation session a node process before
// this one started (ADR 009): followed to its end, or read from its
// file when it is gone, and its verdict recorded by afterEvaluate. An
// item no longer in evaluating on this node is not its to judge: the
// session is stopped and its directory removed.
func (e *Evaluator) AdoptEvaluation(ctx context.Context, item gateway.WorkItem, s FoundSession) error {
	ctx = actorContext(ctx, e.Cfg, gateway.RoleEvaluator)
	if item.Status != "evaluating" || item.NodeID != s.State.NodeID {
		e.logf("item %s: adopted evaluation is no longer this node's (%s); stopped", item.ID, item.Status)
		stopFound(e.logf, "item "+item.ID, s)
		return s.Remove()
	}
	res, err := Resolve(ctx, e.GW, item)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	// the harness the session ran on, by its state's adapter: a Codex
	// pod session's stream is Codex's (ADR 009)
	h, fam := e.Harness, family{model: s.State.Model, claude: s.State.Model}
	if s.State.Harness == AdapterCodex {
		h = e.Codex
		fam = family{codex: true, model: s.State.Model, claude: EvaluateModel(ctx, e.GW, item, e.Model), priority: res.Goal.Priority}
	}
	at, ok := h.(harness.Attacher)
	if !ok {
		stopFound(e.logf, "item "+item.ID, s)
		s.Remove()
		return errNoAttach
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	ctx = WithLockHolder(ctx, label)
	endProxy := adoptProxy(ctx, e.Proxies, e.Credentials, e.MCP, tools.RoleFor(string(harness.KindEvaluate)), e.runtimes(), gateway.SessionRef{Kind: string(harness.KindEvaluate), ItemID: item.ID, GoalID: item.GoalID}, s, "item "+label, e.logf)
	defer endProxy(ctx)
	// adopted, the node holds the evaluation again: it heartbeats until
	// the verdict, as a started one does
	ctx, hb := e.startHeartbeat(ctx, item.ID, label, evalPhaseSession)
	defer hb.end()
	e.logf("item %s: adopted its %s evaluation on %s (pid %d)", label, s.Liveness(), fam.logName(), s.State.PID)
	result, err := at.Attach(ctx, s.Dir, nil)
	changed, cfg := e.touched(ctx, label, s.State.Workspace, res.BaseRef(), defaultBranchOf(res), item.Outcome == gateway.OutcomeAlreadyDone)
	return e.claimLost(hb, label, e.afterEvaluate(ctx, evalRun{item: item, res: res, label: label, model: s.State.Model, fam: fam, dir: s.State.Workspace, adopted: true, prior: e.priorAttempts(ctx, item, label), changed: changed, setting: cfg.setting(), protected: cfg.ProtectedPaths}, result, err))
}

// opinion is the second evaluation's outcome: its verdict and notes, or
// why there is none (unavailable), and what it cost either way.
type opinion struct {
	passed      bool
	notes       string
	cost        float64
	unavailable string
	// killed is the sentence for what the session's end killed ("" when
	// nothing), logged already
	killed string
	// result is the session's, for its tokens in the evaluation's report
	result harness.Result
	// acct is a Claude second opinion's own account, whose report the
	// evaluation's carries (nil: a Codex one, on the harness's account)
	acct *SessionAccount
	// clauses are the acceptance clauses it failed, empty on a pass
	clauses []harness.FailedClause
	// called: its verdict came from a mesh_verdict call (ADR-023)
	called bool
}

// secondOpinion runs the evaluation's job on the other family (on):
// Codex after Claude, unless its account is paced or stopped; Claude
// after Codex, on an account of its own. A session that errors, times
// out, reaches no verdict, saw no context (toolBroken) or ran in a
// sandbox that blocked every command (Result.SandboxBroken) is
// unavailable, and the first verdict stands. role names it in the job's
// label and node.log: "second opinion", or "evaluation" for the one
// that stands in for a session that saw no context.
func (e *Evaluator) secondOpinion(ctx context.Context, ev evalRun, on family, job harness.Job, role string) opinion {
	label := ev.label
	if role == "second opinion" {
		setEvalPhase(ctx, evalPhaseSecondOpinion)
	} else {
		setEvalPhase(ctx, evalPhaseSession)
	}
	h := e.Harness
	var acct *SessionAccount
	sc, _ := e.Scopes.ByOrg(ev.item.OrgID)
	ref := gateway.SessionRef{Kind: string(harness.KindEvaluate), ItemID: ev.item.ID, GoalID: ev.item.GoalID}
	ctx = withSubject(ctx, sc, ev.item.ProjectID, "")
	var open func() (*SessionAccount, error)
	if on.codex {
		if _, why, ok := e.codexForSecond(ev.res.Goal.Priority); !ok {
			return opinion{unavailable: why}
		}
		h = e.Codex
		// an issued Codex account, opened as a first Codex evaluation's is
		open = func() (*SessionAccount, error) {
			return e.Credentials.Open(withChooser(ctx, e.codexChooser(ev.res.Goal.Priority, false)), ref, sc.NodeID, AdapterCodex)
		}
	} else {
		open = func() (*SessionAccount, error) {
			return e.Credentials.Open(ctx, ref, sc.NodeID, AdapterClaudeCode)
		}
	}
	if open != nil {
		a, err := open()
		if err != nil {
			return opinion{unavailable: truncate(harness.Redact(err.Error(), job.Secrets...), 300)}
		}
		defer a.Cleanup()
		if on.codex {
			a.Model = on.model
		}
		a.Apply(&job)
		acct = a
	}
	job.Model = on.model
	job.Label = label + " " + role
	if on.codex {
		if why := e.preflight(ctx, job, acct); why != "" {
			return opinion{unavailable: "preflight " + why}
		}
	}
	e.logf("item %s: %s on %s", label, role, on.logName())
	result, err := h.Run(ctx, job, nil)
	op := opinion{cost: result.CostUSD, result: result, acct: acct,
		killed: logKilled(e.logf, "item "+label, result.KilledPerSession(), result.KillError)}
	if acct != nil {
		acct.Report(result)
	}
	switch {
	case ctx.Err() != nil:
		op.unavailable = ctx.Err().Error()
	case errors.Is(err, context.DeadlineExceeded):
		op.unavailable = fmt.Sprintf("the session ran out of time (%s)", e.Timeout)
	case err != nil:
		op.unavailable = truncate(acct.Redact(harness.Redact(err.Error(), job.Secrets...)), 300)
		if _, ok := harness.AsUnavailable(err); ok {
			reportFailure(e.Tripline, tripline.HarnessUnavailable, errors.New(op.unavailable), ev.item, label, repoOf(ev.res), gateway.RoleEvaluator)
		}
	default:
		if result.SandboxBroken != "" {
			op.unavailable = "sandbox broken (" + acct.Redact(harness.Redact(result.SandboxBroken, job.Secrets...)) + ")"
			reportFailure(e.Tripline, tripline.SandboxBroken, errors.New(op.unavailable), ev.item, label, repoOf(ev.res), gateway.RoleEvaluator)
			break
		}
		if blind, broken := e.toolBroken(result, acct, false); broken {
			op.unavailable = "saw no context (" + harness.Redact(blind, job.Secrets...) + ")"
			break
		}
		passed, notes, clauses, called, ok := verdictOf(result, acct.Redact)
		if !called {
			e.logf("item %s: %s: %s", label, role, noVerdictCallLine)
		}
		if !ok {
			op.unavailable = "no verdict: " + truncate(acct.Redact(harness.Redact(result.Text, job.Secrets...)), 300)
			break
		}
		op.passed, op.called = passed, called
		op.notes = strings.TrimSpace(harness.Redact(notes, job.Secrets...))
		if op.notes == "" {
			op.notes = "(no notes)"
		}
		op.clauses = clauses
	}
	return op
}

// noVerdictCallLine is logged once per evaluator session that made no
// mesh_verdict call, its final message parsed instead (ADR-023).
const noVerdictCallLine = "no mesh_verdict call; parsed the final message"

// verdictOf reads a session's verdict: its mesh_verdict call's input when
// it made one with a readable passed (harness.VerdictCall; called true),
// accepted whatever the final message says, else the final message's
// VERDICT line, the notes above it and its FAILED lines, as before
// (ADR-023, finish by tool call). ok is false when neither gives one —
// no verdict. redact is applied to what either says.
func verdictOf(result harness.Result, redact func(string) string) (passed bool, notes string, clauses []harness.FailedClause, called, ok bool) {
	if in, has := result.WorkCalls["mesh_verdict"]; has {
		if passed, notes, clauses, ok := harness.VerdictCall(in); ok {
			for i := range clauses {
				clauses[i].Clause, clauses[i].Satisfy = redact(clauses[i].Clause), redact(clauses[i].Satisfy)
			}
			return passed, redact(notes), clauses, true, true
		}
	}
	text := redact(result.Text)
	passed, notes, ok = harness.ParseVerdict(text)
	if !ok {
		return false, "", nil, false, false
	}
	return passed, notes, harness.ParseFailedClauses(text), false, true
}

// ErrNoContext is an evaluation that saw no context — its built-in
// context server, or a required console tool, never answered
// (toolBroken), or Codex's sandbox blocked every command it ran
// (Result.SandboxBroken) — and could not be evaluated on the other
// family either:
// nothing is recorded, and the item waits in evaluating for a later tick
// (ADR 011, a tool failure is never a verdict).
var ErrNoContext = errors.New("the evaluation saw no context, and the other family could not evaluate it")

// unreadable is why a session's text is no verdict whatever it says —
// why, for the notes' `no verdict — <why>`, and what, for node.log's
// `evaluation on <family> <what>` — or false when it may be read for
// one: Codex's sandbox would not start, so every command it ran failed
// (`sandbox: <the bwrap line>`), or its tests could not bind a socket
// (`sandbox: <the socket line>`; ADR 011 amendments 2026-09-29), or it saw
// no context (toolBroken).
func (e *Evaluator) unreadable(res harness.Result, acct *SessionAccount, adopted bool) (why, what string, ok bool) {
	if res.SandboxBroken != "" {
		why = "sandbox: " + truncate(acct.Redact(res.SandboxBroken), 200)
		return why, "ran in a sandbox that blocked every command (" + why + ")", true
	}
	if blind, broken := e.toolBroken(res, acct, adopted); broken {
		return blind, "saw no context (" + blind + ")", true
	}
	return "", "", false
}

// toolBroken names the tool a session could not use and its first
// error, capped at 200 characters: a built-in server of the evaluator's
// (MCP: the context server) or a required console tool the session was
// served that errored and was never reached (ToolStat.Reached: no clean
// call and no call the server answered itself) — every call refused
// before the server, or an error at startup with no call at all. A tool
// that answered was reached: `not found` for a mesh_document slug it
// does not have is the tool working, not a broken one (ADR-011's
// amendment 2026-09-29, a tool that answered was reached). adopted is a
// session an older node process started (AdoptEvaluation): only there
// may the built-in carry its old name (tools.IsBuiltin).
func (e *Evaluator) toolBroken(res harness.Result, acct *SessionAccount, adopted bool) (string, bool) {
	broken := func(name string) (string, bool) {
		s := res.Tools[name]
		if s == nil || !(s.Errors > 0 && !s.Reached()) {
			return "", false
		}
		msg := s.FirstError
		if msg == "" {
			msg = fmt.Sprintf("%d of %d calls errored", s.Errors, s.Calls)
		}
		return acct.Redact(msg), true
	}
	// the node's own servers and, for an adopted session, the built-in
	// under any name it has had: an evaluation an older node started
	// under its old name reports that name, not tools.Builtin
	// (tools.IsBuiltin: mesh-context, or the name before it). A session this
	// process started was rendered runsheet, so a server by the old
	// name in its result is a console tool: broken only if required, below.
	names := map[string]bool{}
	for name := range e.MCP {
		names[name] = true
	}
	if adopted {
		for name := range res.Tools {
			if tools.IsBuiltin(name) {
				names[name] = true
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if msg, ok := broken(name); ok {
			return name + ": " + truncate(msg, 200), true
		}
	}
	if acct != nil {
		for _, t := range acct.used {
			if !t.Required {
				continue
			}
			if msg, ok := broken(t.Name); ok {
				return t.Name + ": " + truncate(redactToolValues(msg, t), 200), true
			}
		}
	}
	return "", false
}

// canRetry says whether an evaluation that saw no context can run on
// the other family (on), or why not: an adopted session's prompt is not
// kept, a pinned family is the only one the item takes, and Codex needs
// its harness and an account with headroom.
func (e *Evaluator) canRetry(ev evalRun, on family) (string, bool) {
	switch {
	case ev.second == nil:
		return "the evaluation was adopted after a restart, and its prompt is not kept", false
	case ev.fam.pinned:
		return "the item pins " + ev.fam.fullName(), false
	case on.codex && !e.hasCodex():
		return "the node has no Codex harness or account", false
	case on.codex:
		if _, why, ok := e.codexForSecond(ev.res.Goal.Priority); !ok {
			return why, false
		}
	}
	return "", true
}

// blind is ErrNoContext, with line said once for the item until an
// evaluation of it saw its context.
func (e *Evaluator) blind(itemID, line string) error {
	if e.Blind.First(itemID) {
		e.logf("%s; it stays in evaluating for a later tick (said once for the item)", line)
	}
	return ErrNoContext
}

// codexForSecond is the issued Codex account a second opinion for a
// goal of that priority runs on, or why there is none: none of
// CodexAccounts has headroom.
func (e *Evaluator) codexForSecond(priority string) (AccountConfig, string, bool) {
	a, ok := e.chooseCodex(priority, false)
	if !ok {
		return AccountConfig{}, "no Codex account has headroom", false
	}
	return a, "", true
}

// bothNotes labels the two verdicts, the first family's first, each by
// its family and model, so a builder sees both, truncated together to
// n: each gets half, and one shorter than its half leaves the rest to
// the other, so neither is cut out.
func bothNotes(firstOn family, first string, firstPassed bool, secondOn family, second string, secondPassed bool, n int) string {
	firstHead := firstOn.name() + " (first evaluation, " + firstOn.shortModel() + "): " + verdictLine(firstPassed) + "\n"
	secondHead := secondOn.name() + " (second opinion, " + secondOn.shortModel() + "): " + verdictLine(secondPassed) + "\n"
	// the blank line between them, and the two bytes more truncate's
	// "…" takes than the byte it replaces, in each
	room := n - len(firstHead) - len(secondHead) - 2 - 4
	a, b := room/2, room-room/2
	if len(first) < a {
		b += a - len(first)
	} else if len(second) < b {
		a += b - len(second)
	}
	return firstHead + truncateNotes(first, a) + "\n\n" + secondHead + truncateNotes(second, b)
}

// evalDecision is what the evaluate path decides one evaluator or two
// on (goal one-evaluator-for-ordinary-work; ADR 005's and ADR 011's
// amendments 2026-10-02): the goal's priority, what the item touched
// that is protected, the first verdict and the project's setting.
type evalDecision struct {
	// Priority is the goal's.
	Priority string
	// ProtectedFiles are the item's touched files the platform matched to
	// a protected rule; ProtectedRulePaths that rule's globs.
	ProtectedFiles     []string
	ProtectedRulePaths []string
	// ProtectedPaths are the project's own protected-path globs.
	ProtectedPaths []string
	// Changed are the files the evaluated diff changed.
	Changed []string
	// FirstPassed is the first verdict, FailedClauses the clauses it
	// failed (none on a pass).
	FirstPassed   bool
	FailedClauses []harness.FailedClause
	// Setting is the project's second_opinion: "always", else the rule.
	Setting string
}

// secondOpinionAlways is the second_opinion setting that keeps two
// evaluators on every item.
const secondOpinionAlways = "always"

// oneEvaluatorReason is the reason a normal or low item gets one verdict.
const oneEvaluatorReason = "normal/low goal, no protected path"

// secondOpinionWanted is the rule: an urgent or high goal, a protected
// touch, a first FAIL on a correctness clause (one that is not a reading
// of the host, ADR-020) or second_opinion: always gets a second
// evaluator; anything else one. The reason is the first that held.
func secondOpinionWanted(d evalDecision) (bool, string) {
	if gateway.PriorityRank(d.Priority) <= gateway.PriorityRank("high") {
		return true, "urgent/high goal"
	}
	if len(d.ProtectedFiles) > 0 {
		return true, "protected path " + d.ProtectedFiles[0]
	}
	for _, f := range d.Changed {
		for _, g := range append(append([]string(nil), d.ProtectedRulePaths...), d.ProtectedPaths...) {
			if globMatch(g, f) {
				return true, "protected path " + f
			}
		}
	}
	if !d.FirstPassed {
		for _, c := range d.FailedClauses {
			if !hostReadingClause(c.Clause + ". " + c.Satisfy) {
				return true, "first verdict FAIL on a correctness clause"
			}
		}
	}
	if d.Setting == secondOpinionAlways {
		return true, "second_opinion: always"
	}
	return false, oneEvaluatorReason
}

// projectProtectedPaths are the project's protected-path globs, none
// when the project was not read.
func projectProtectedPaths(res *Resolved) []string {
	if res == nil || res.Project == nil {
		return nil
	}
	return res.Project.ProtectedPaths
}

// unionClauses is a's failed clauses and those of b no clause of a
// names already.
func unionClauses(a, b []harness.FailedClause) []harness.FailedClause {
	out := slices.Clone(a)
	for _, c := range b {
		if !slices.ContainsFunc(out, func(o harness.FailedClause) bool {
			return strings.EqualFold(strings.TrimSpace(o.Clause), strings.TrimSpace(c.Clause))
		}) {
			out = append(out, c)
		}
	}
	return out
}

func verdictLine(passed bool) string {
	if passed {
		return harness.VerdictPass
	}
	return harness.VerdictFail
}

func codexModelName(m string) string {
	if m == "" {
		return " (its default model)"
	}
	return " " + m
}

// family is the model family an evaluation session runs on, Codex or
// Claude, and its model (ADR 011).
type family struct {
	codex bool
	// pinned: the item named the family (EvaluatorPin); it waits rather
	// than fall back to the other
	pinned bool
	// model is the session's; claude is the item's Claude evaluate model
	// (EvaluateModel) whichever family runs, which a Codex one maps from
	// and a Claude second opinion runs on
	model, claude string
	priority      string
}

func (f family) name() string {
	if f.codex {
		return "Codex"
	}
	return "Claude"
}

// logName is how node.log and the notes' last line name the session:
// Codex and its model, or the Claude model as it always has.
func (f family) logName() string {
	if f.codex {
		return "Codex" + codexModelName(f.model)
	}
	return modelName(f.model)
}

// fullName is the family and its model: "Codex gpt-6-sol", "Claude
// sonnet".
func (f family) fullName() string {
	if f.codex {
		return f.logName()
	}
	return "Claude " + modelName(f.model)
}

// shortModel is the model in a verdict's label.
func (f family) shortModel() string {
	switch {
	case f.model != "":
		return f.model
	case f.codex:
		return "its default model"
	}
	return "the CLI's default model"
}

// other is the family a second opinion runs on: Claude after Codex, on
// the item's Claude evaluate model; Codex after Claude, on its mapping.
func (f family) other(e *Evaluator) family {
	if f.codex {
		return family{model: f.claude, claude: f.claude, priority: f.priority}
	}
	return family{codex: true, model: e.codexModel(f.claude), claude: f.claude, priority: f.priority}
}

// preflight is why the Codex session job would not reach the built-in
// context server (Evaluator.CodexPreflight), with every secret the
// session carries taken out; "" when it would, or with no preflight.
func (e *Evaluator) preflight(ctx context.Context, job harness.Job, acct *SessionAccount) string {
	if e.CodexPreflight == nil {
		return ""
	}
	err := e.CodexPreflight(ctx, job)
	if err == nil {
		return ""
	}
	return truncate(acct.Redact(harness.Redact(err.Error(), job.Secrets...)), 300)
}

func preflightKey(itemID string) string { return "preflight:" + itemID }

// preflightWaits is ErrFamilyWaits for an item pinned to Codex whose
// preflight failed: no verdict, it waits in evaluating, said once for
// the item until a preflight passes.
func (e *Evaluator) preflightWaits(itemID, label, model, why string) error {
	if e.Waits.First(preflightKey(itemID)) {
		e.logf("item %s: pinned to Codex %s, and the Codex preflight failed (%s): it waits in evaluating for a later tick (said once for the item)", label, model, why)
	}
	return fmt.Errorf("%w: Codex preflight failed: %s", ErrFamilyWaits, why)
}

// ErrFamilyWaits is an item pinned to a family none of whose accounts
// has headroom: not evaluated, it waits in evaluating for a later tick
// (ADR 011 decision 2), and is said once per item (Evaluator.Waits).
var ErrFamilyWaits = errors.New("the evaluator's pinned family has no account with headroom")

// IsCodexModel says a model names a Codex (OpenAI) model: gpt-….
func IsCodexModel(m string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(m)), "gpt-")
}

// EvaluatorPin is the family an item pins its evaluator to (ADR 011
// decision 2), by the first of its evaluate model and its own or its
// agent's model (ItemModel) that names one by a full id: gpt-… pins
// Codex (and is the model), claude-… pins Claude. A bare tier (haiku,
// sonnet, opus) is a strength, not a family — the platform names one
// for nearly every item — so it pins nothing and maps to Codex's tier
// (CodexModel). "" is no pin.
func EvaluatorPin(ctx context.Context, gw Gateway, item gateway.WorkItem) (adapter, model string) {
	for _, m := range []string{item.EvaluateModel, ItemModel(ctx, gw, item)} {
		m = strings.TrimSpace(m)
		switch {
		case IsCodexModel(m):
			return AdapterCodex, m
		case strings.HasPrefix(strings.ToLower(m), "claude-"):
			return AdapterClaudeCode, ""
		}
	}
	return "", ""
}

// family chooses the family the item's evaluation runs on: its pin,
// else Codex when a Codex account has headroom for the goal's priority
// (chooseCodex), else Claude. A Codex pin with no Codex headroom is
// ErrFamilyWaits.
func (e *Evaluator) family(ctx context.Context, item gateway.WorkItem, priority, label string) (family, error) {
	claude := EvaluateModel(ctx, e.GW, item, e.Model)
	fam := family{model: claude, claude: claude, priority: priority}
	switch pin, model := EvaluatorPin(ctx, e.GW, item); pin {
	case AdapterClaudeCode:
		return fam, nil
	case AdapterCodex:
		if _, ok := e.chooseCodex(priority, true); !ok {
			return fam, e.waits(item.ID, label, model)
		}
		return family{codex: true, pinned: true, model: model, claude: claude, priority: priority}, nil
	}
	if _, ok := e.chooseCodex(priority, false); ok {
		return family{codex: true, model: e.codexModel(claude), claude: claude, priority: priority}, nil
	}
	return fam, nil
}

// waits is ErrFamilyWaits for an item pinned to Codex, said once for
// the item until it is evaluated.
func (e *Evaluator) waits(itemID, label, model string) error {
	if e.Waits.First(itemID) {
		e.logf("item %s: pinned to Codex %s, and no Codex account has headroom: it waits in evaluating for a later tick (said once for the item)", label, model)
	}
	return ErrFamilyWaits
}

// openFamily opens the session's account on its family: a Codex one on
// the chosen issued credential (by the same rule, among those the
// claim-time read issues), never the operator's own login; a Claude one
// as ever. An unpinned Codex evaluation none of whose issued credentials
// has headroom after all runs on Claude instead; a pinned one waits.
func (e *Evaluator) openFamily(ctx context.Context, ref gateway.SessionRef, nodeID string, fam family, label string) (*SessionAccount, family, error) {
	if fam.codex {
		acct, err := e.Credentials.Open(withChooser(ctx, e.codexChooser(fam.priority, fam.pinned)), ref, nodeID, AdapterCodex)
		switch {
		case err == nil:
			e.Waits.Forget(ref.ItemID)
			acct.Model = fam.model
			return acct, fam, nil
		case !errors.Is(err, ErrNoAccount) && !isNoCredential(err):
			return nil, fam, err
		case fam.pinned:
			return nil, fam, e.waits(ref.ItemID, label, fam.model)
		}
		e.logf("item %s: no issued Codex credential has headroom after all; evaluating on Claude", label)
		fam = family{model: fam.claude, claude: fam.claude, priority: fam.priority}
	}
	acct, err := e.Credentials.Open(ctx, ref, nodeID, AdapterClaudeCode)
	return acct, fam, err
}

// CodexHeadroom says whether a Codex account has headroom for an
// unpinned evaluation of that priority (chooseCodex).
func (e *Evaluator) CodexHeadroom(priority string) bool {
	_, ok := e.chooseCodex(priority, false)
	return ok
}

// hasCodex says the evaluator has Codex to run on at all: its harness,
// and an issued Codex account (CodexAccounts).
func (e *Evaluator) hasCodex() bool {
	return e.Codex != nil && e.CodexAccounts != nil && len(e.CodexAccounts()) > 0
}

// chooseCodex is the Codex account an evaluation of that priority runs
// on (codexChooser over CodexAccounts); none without a Codex harness or
// any Codex account.
func (e *Evaluator) chooseCodex(priority string, pinned bool) (AccountConfig, bool) {
	if e.Codex == nil || e.CodexAccounts == nil {
		return AccountConfig{}, false
	}
	accounts := e.CodexAccounts()
	if len(accounts) == 0 {
		return AccountConfig{}, false
	}
	return e.codexChooser(priority, pinned)(accounts)
}

// codexChooser judges Codex accounts by their readings under codex:
// and the node's thresholds, as ChooseAccount judges Claude's for the
// priority: a paced account takes only urgent and high work, a stopped
// one nothing. A pinned evaluation — work already built with nowhere
// else to go — also takes a paced subscription, as accountToFinish does.
func (e *Evaluator) codexChooser(priority string, pinned bool) func([]AccountConfig) (AccountConfig, bool) {
	return func(accounts []AccountConfig) (AccountConfig, bool) {
		var readings map[string]harness.RateLimit
		if e.Limits != nil {
			readings = e.Limits.CodexReadings()
		}
		now, cfg := time.Now(), e.thresholds()
		if a, ok := ChooseAccount(now, accounts, readings, cfg, priority, true); ok || !pinned {
			return a, ok
		}
		subs := slices.DeleteFunc(slices.Clone(accounts), func(a AccountConfig) bool { return a.Kind == AccountAPI || !a.Serves(priority) })
		return ChooseAccount(now, subs, readings, cfg, "high", true)
	}
}

func (e *Evaluator) codexModel(claude string) string {
	if e.CodexModel != nil {
		return e.CodexModel(claude)
	}
	return CodexConfig{}.CodexModel(claude)
}

func (e *Evaluator) thresholds() LimitsConfig {
	if e.Thresholds != nil {
		return e.Thresholds()
	}
	return LimitsConfig{}
}

// OncePer remembers what has been said once, by key; a nil one
// remembers nothing (First is always true).
type OncePer struct {
	mu   sync.Mutex
	seen map[string]bool
}

// First says whether key is new, and remembers it.
func (o *OncePer) First(key string) bool {
	if o == nil {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen[key] {
		return false
	}
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	o.seen[key] = true
	return true
}

// Forget lets key be said again.
func (o *OncePer) Forget(key string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	delete(o.seen, key)
	o.mu.Unlock()
}

// Cents rounds a dollar cost to whole cents.
func Cents(usd float64) int {
	if usd <= 0 {
		return 0
	}
	return int(math.Round(usd * 100))
}

// keptAt is " kept at <dir>" when result names where its whole event
// stream and stderr are kept (a Codex session with harness.Codex's
// WorkRoot set), else "".
func keptAt(result harness.Result) string {
	if result.KeptSessionDir == "" {
		return ""
	}
	return " kept at " + result.KeptSessionDir
}

// hasToolError is whether any MCP server the session called, or was
// served but never reached, ended in error (harness.Result.Tools).
func hasToolError(result harness.Result) bool {
	for _, stat := range result.Tools {
		if stat != nil && stat.Errors > 0 {
			return true
		}
	}
	return false
}

// contextBudget is the context budget for e's sessions: live, through
// e.ContextBudget, when set; else Cfg's own.
func (e *Evaluator) contextBudget() ContextConfig {
	if e.ContextBudget != nil {
		return e.ContextBudget()
	}
	if e.Cfg == nil {
		return ContextConfig{}.Effective()
	}
	return e.Cfg.Context.Effective()
}

// releaseInfrastructure answers an evaluation whose token could not be
// written into its checkout (NewItemCred's *WorkspaceError): the item
// is released as an infrastructure hold (ReleaseOnInfrastructure) and
// held off this node for WorkspaceReleaseHold. A release the platform
// refuses — it releases only a claimed or running item — leaves the
// item in evaluating, never failed, as an evaluation that could not
// start always has.
func (e *Evaluator) releaseInfrastructure(ctx context.Context, item gateway.WorkItem, label string, we *WorkspaceError) error {
	sc, _ := e.Scopes.ByOrg(item.OrgID)
	stopHeartbeat(ctx)
	err := ReleaseOnInfrastructure(ctx, e.GW, item.ID, sc.NodeID, label, we, func(string) {
		e.logf("item %s: not evaluated, the node's condition; left in evaluating", label)
	}, e.logf)
	if ctx.Err() == nil {
		reportFailure(e.Tripline, tripline.Infrastructure, err, item, label, we.Repo, gateway.RoleEvaluator)
		holdInfrastructure(e.Declines, item.ID, label, we, e.logf)
	}
	return err
}
