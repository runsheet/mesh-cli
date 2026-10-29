package node

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/jobresult"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
	"projectmesh.io/mesh-cli/internal/tripline"
)

// Evaluator is the second fresh session an item gets on the node
// (mesh-specs trd §3.4): the finished branch checked out, its diff from
// the base, the acceptance, an adversarial review — and its verdict
// handed by the session to the product's own tool (the brief's
// finishTool, mesh_verdict); the node then closes the job with its notes
// tail (runsheet ADR 077). The platform opens the pull request on a pass
// and readies the item again on a fail (with the notes for the next
// builder), so the evaluator changes nothing itself. An item finished
// already_done (mesh ADR 003 decision 1) is checked on the base ref
// instead — its evidence and acceptance, no branch and no diff — and the
// same tool confirms it (the platform marks it merged, with no pull
// request) or readies it again.
type Evaluator struct {
	GW Gateway
	// noGuidance logs once per kind whose brief carries no guidance
	// (ADR-041).
	noGuidance NoGuidance
	Harness    harness.Harness
	WS         Workspacer
	Cfg        *Config
	Scopes     Scopes
	// Layout, when set, is the node's job and connection trees (ADR-039):
	// each item's checkout, scratch, kept stream and MCP files go in its
	// job, its clone and store in its connection's caches, WorkRoot
	// unused for them. Nil keeps WorkRoot (the tests' layout).
	Layout   *Layout
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
	// NoVerdict says once per run that the platform takes no no-verdict
	// outcome, so an evaluation that reached none waits in evaluating
	// (nil: every time).
	NoVerdict *OncePer
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
	nodeID := nodeIDOf(e.Scopes, item.OrgID, e.Cfg)
	ctx = gateway.WithNodeID(ctx, nodeID)
	// the node's own outcome closes the job: the product fails the item
	// on the clause it names for a branch it cannot evaluate (runsheet
	// ADR 077, the node's outcome rides the result)
	res := map[string]any{"outcome": "failed", "cause": notes}
	if _, err := e.GW.FinishJob(ctx, nodeID, EvaluateKind, gateway.JobSubject{Type: "item", ID: item.ID}, map[string]any{"result": res}); err != nil {
		_ = handInRefused(e.logf, nil, label, "FinishJob", err)
		return fmt.Errorf("record verdict: %w", err)
	}
	return nil
}

// evaluationNotYours reports whether err is issueItemRepoToken or
// the job's close refused because another node already holds this
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
	// the node a finish is handed back by (finishJob, runsheet ADR 061)
	ctx = gateway.WithNodeID(ctx, nodeIDOf(e.Scopes, item.OrgID, e.Cfg))
	ctx = gateway.WithJobKind(ctx, "runsheet/evaluate")
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
	spec, err := evalSpec(e.Layout, e.Scopes, item, label, EvalWorkspaceSpec(e.WorkRoot, res.Goal.Slug, item.Seq, res.Repo.FullName(), branch, res.BaseRef()))
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
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
		// the would-be merge (runsheet ADR 077): a brief naming onto has
		// the change evaluated on it, and diffed from it; a brief without
		// one, as always
		if code := jobBriefOf(ctx, e.GW, EvaluateKind, item.ID).Code; code.Onto != nil {
			onto, conflict := e.evaluateOnto(ctx, dir, label, code)
			if conflict != "" {
				return e.ontoConflict(ctx, item, label, conflict)
			}
			if onto != "" {
				diffBase = onto
			}
		}
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
	// the verdict goes to the product's own tool (runsheet ADR 077, the
	// evaluation policy is runsheet's): the node hosts no verdict tool
	brief := jobBriefOf(ctx, e.GW, EvaluateKind, item.ID)
	if brief.FinishTool == "" {
		return fmt.Errorf("the %s brief names no finish tool to hand the verdict to", EvaluateKind)
	}
	if brief.Guidance == "" {
		e.noGuidance.Note(EvaluateKind, e.logf)
	}
	in := harness.EvaluateInput{
		ItemID: item.ID, GoalSlug: res.Goal.Slug, GoalTitle: res.Goal.Title, GoalAcceptance: res.Goal.Acceptance,
		Seq: item.Seq, Title: item.Title, Brief: item.Brief, Acceptance: item.Acceptance,
		Repo: res.Repo.FullName(), Branch: branch, BaseRef: diffBase, Diff: diff,
		ContextDir: contextDir, ContextFiles: files, Project: orgName + "/" + res.Project.Slug, MeshContext: len(e.MCP) > 0,
		AlreadyDone: alreadyDone, Evidence: item.Evidence,
		PriorAttempts: prior,
		Guidance:      brief.ProductGuidance(false), Finish: brief.HandInSection(),
	}
	model := fam.model
	budget, budgetSource := e.budgetWithSource(res.Goal, model)
	e.logf("item %s: evaluate budget $%.2f (%s)", label, budget, budgetSource)
	kept, err := keptRootFor(e.Layout, item.ID)
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
	scratchRoot, err := scratchRootFor(e.Layout, item.ID, e.WorkRoot)
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
	pod, err := openOrgSessionPod(ctx, e.Sandbox, e.Layout, e.Scopes, item.OrgID, dir, job.Label, harness.KindEvaluate, sandbox.GitIdentity{}, e.logf)
	defer pod.Close() // nothing, once the harness detached the session (Session.Detach)
	if re := RuntimeErrorOf(err); re != nil {
		return e.runtimeNoVerdict(ctx, evalRun{item: item, res: res, label: label, model: model, fam: fam, dir: dir, acct: acct}, re, harness.Result{})
	}
	if errors.Is(err, ErrHostServesSeveralOrgs) {
		// no sandbox on a node serving several orgs: never on the host
		// (ADR 066, decision 10). An item in evaluating can be neither
		// released nor reported on, so it is left unevaluated and held
		// off this node, as a required service that did not come up is
		noteHostFallback(err, e.logf)
		reason := HostFallbackReason(err)
		e.Declines.HoldFor(item.ID, reason, time.Now(), WorkspaceReleaseHold)
		e.logf("item %s: not evaluated, %s", label, reason)
		return err
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
	var st SessionTools
	preflight := ""
	for {
		job = base
		job.Env = slices.Clone(base.Env)
		model, job.Model = fam.model, fam.model
		job.Meta = harness.SessionMeta{NodeID: sc.NodeID, ItemID: item.ID, GoalID: item.GoalID, Attempt: item.Attempts, TempDirs: sessionTempDirs(job, acct), LoginCache: acct.loginCache()}
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
		job.BriefServers = briefServers(ctx, e.GW, EvaluateKind, item.ID)
		closeProxy, err := st.UseVia(e.Proxies, mcpRootFor(e.Layout, item.ID), &job, e.MCP, acct)
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
	return e.afterEvaluate(ctx, evalRun{item: item, res: res, label: label, model: model, fam: fam, dir: dir, acct: acct, preflight: preflight, omitted: st.OmittedNote(), missing: pod.MissingNote(), prior: prior}, result, err)
}

// EvaluateKind is runsheet's evaluate kind, the job an evaluation is.
const EvaluateKind = "runsheet/evaluate"

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
}

// afterEvaluate is what follows an evaluation session, however this
// process came to hold it: the session handed its verdict (or that it
// reached none) to the product's own tool, and the node closes the job
// with finishJob's result, the node's own facts as its notes_tail, which
// the product appends to the evaluation (runsheet ADR 077: the
// evaluation policy — evaluators, the same-clause line, splits — is the
// product's). The product refuses to close a job no verdict reached. A
// cancelled ctx leaves the session running for the next node to adopt.
func (e *Evaluator) afterEvaluate(ctx context.Context, ev evalRun, result harness.Result, err error) error {
	item, label, acct := ev.item, ev.label, ev.acct
	// what the agent left running goes in the notes tail; logged before a
	// done ctx returns: a deadline kills the unit too, and a detach kills
	// nothing
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
	if ev.fam.codex && acct != nil {
		acct.Model = ev.fam.model
	}
	usage := acct.Report(result)
	// the node's own facts, which runsheet appends to the evaluation the
	// session recorded: who judged, what it ran without, what it left
	// running, and a Codex preflight that moved it to Claude
	tail := "(evaluated on " + ev.fam.fullName() + ")"
	if ev.preflight != "" {
		tail = "(evaluated on " + ev.fam.fullName() + "; the Codex preflight failed: " + truncate(ev.preflight, 200) + ")"
	}
	if ev.omitted != "" {
		tail += "\n" + truncate(ev.omitted, 500)
	}
	if ev.missing != "" {
		tail += "\n" + truncate(ev.missing, 500)
	}
	if note := acct.UseNote(result); note != "" {
		e.logf("item %s: %s", label, note)
		tail += "\n" + truncate(note, 500)
	}
	if killed != "" {
		tail += "\n" + truncate(killed, 500)
	}
	if result.KeptSessionDir != "" && hasToolError(result) {
		e.logf("item %s: a tool errored%s", label, keptAt(result))
	}
	// the usage record keeps its goal priority and evaluator count (one:
	// a second opinion is the product's to offer, runsheet ADR 077)
	acct.SetEvaluation(ev.res.Goal.Priority, 1)
	// no heartbeat after the verdict
	stopHeartbeat(ctx)
	nodeID := nodeIDOf(e.Scopes, item.OrgID, e.Cfg)
	ctx = gateway.WithNodeID(ctx, nodeID)
	// the session's cost and usage ride the result too: the session's
	// verdict call carries neither, and a session's usage rides the
	// command that ends it
	res := map[string]any{"notes_tail": truncate(tail, 2000), "cost_cents": Cents(result.CostUSD)}
	if usage != nil {
		res["usage"] = usage
	}
	if _, ferr := e.GW.FinishJob(ctx, nodeID, EvaluateKind, gateway.JobSubject{Type: "item", ID: item.ID}, map[string]any{"result": res}); ferr != nil {
		if evaluationNotYours(ferr) {
			// another node converged on the evaluation while this session
			// ran: its verdict is the one of record
			left.Forget(item.ID)
			e.logf("item %s: evaluation closed elsewhere: another node holds it", label)
			return nil
		}
		_ = handInRefused(e.logf, acct.Redact, label, "FinishJob", ferr)
		return fmt.Errorf("evaluator session (%s) ended; the product refused to close the job: %w", result.SessionID, ferr)
	}
	left.Forget(item.ID)
	e.logf("item %s: evaluation handed in through the product's tool on %s; job closed ($%.2f)", label, ev.fam.fullName(), result.CostUSD)
	return nil
}

// noVerdict records an evaluation the node could not run to a verdict —
// its container runtime would not start the session — as the job's own
// no_verdict outcome (runsheet ADR 077: the product records it as a
// no-verdict, never a fail, with the session's cost and usage when one
// ran). A platform that does not take the outcome (unsupported, or one
// that reads the result as the session's close and refuses it) records
// nothing: said once per run, and fallback is the error the item waits
// in evaluating with.
func (e *Evaluator) noVerdict(ctx context.Context, ev evalRun, reason string, cost float64, usage *gateway.UsageReport, fallback func() error) error {
	reason = truncate(reason, 4000)
	stopHeartbeat(ctx)
	nodeID := nodeIDOf(e.Scopes, ev.item.OrgID, e.Cfg)
	ctx = gateway.WithNodeID(ctx, nodeID)
	res := map[string]any{"outcome": "no_verdict", "reason": reason, "cost_cents": Cents(cost)}
	if usage != nil {
		res["usage"] = usage
	}
	if ev.fam.model != "" {
		res["evaluated_model"] = ev.fam.model
	}
	_, err := e.GW.FinishJob(ctx, nodeID, EvaluateKind, gateway.JobSubject{Type: "item", ID: ev.item.ID}, map[string]any{"result": res})
	if gateway.IsUnsupported(err) || (err != nil && strings.Contains(err.Error(), "call mesh_verdict first")) {
		if e.NoVerdict.First("") {
			e.logf("the platform takes no no-verdict outcome; a no-verdict evaluation waits in evaluating (said once per run)")
		}
		return fallback()
	}
	var redact func(string) string
	if ev.acct != nil {
		redact, err = ev.acct.Redact, ev.acct.Carried(err)
	}
	if err := handInRefused(e.logf, redact, ev.label, "FinishJob", err); err != nil {
		return fmt.Errorf("record no verdict: %w", err)
	}
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
	// the node a finish is handed back by (finishJob, runsheet ADR 061)
	ctx = gateway.WithNodeID(ctx, s.State.NodeID)
	ctx = gateway.WithJobKind(ctx, "runsheet/evaluate")
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
	// a fresh holder for the adopted session (ADR-041): a verdict it
	// handed the node before this one lived with that node
	holder, err := jobresult.New(EvaluateKind, jobresult.FinishVerdict, "")
	if err != nil {
		return err
	}
	endProxy := adoptProxy(ctx, e.Proxies, e.Credentials, e.MCP, tools.RoleFor(string(harness.KindEvaluate)), e.runtimes(), gateway.SessionRef{Kind: string(harness.KindEvaluate), ItemID: item.ID, GoalID: item.GoalID}, s, "item "+label, holder.Tools(), e.logf)
	defer endProxy(ctx)
	// adopted, the node holds the evaluation again: it heartbeats until
	// the verdict, as a started one does
	ctx, hb := e.startHeartbeat(ctx, item.ID, label, evalPhaseSession)
	defer hb.end()
	e.logf("item %s: adopted its %s evaluation on %s (pid %d)", label, s.Liveness(), fam.logName(), s.State.PID)
	result, err := at.Attach(ctx, s.Dir, nil)
	return e.claimLost(hb, label, e.afterEvaluate(ctx, evalRun{item: item, res: res, label: label, model: s.State.Model, fam: fam, dir: s.State.Workspace, adopted: true, prior: e.priorAttempts(ctx, item, label)}, result, err))
}

// ErrNoContext is an evaluation that saw no context — its built-in
// context server, or a required console tool, never answered
// (toolBroken), or Codex's sandbox blocked every command it ran
// (Result.SandboxBroken) — and could not be evaluated on the other
// family either:
// nothing is recorded, and the item waits in evaluating for a later tick
// (ADR 011, a tool failure is never a verdict).
var ErrNoContext = errors.New("the evaluation saw no context, and the other family could not evaluate it")

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
