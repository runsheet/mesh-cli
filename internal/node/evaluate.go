package node

import (
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
	GW       Gateway
	Harness  harness.Harness
	WS       Workspacer
	Cfg      *Config
	Scopes   Scopes
	WorkRoot string
	// MCP are the MCP servers the session gets (mesh-context).
	MCP map[string]any
	// MaxBudgetUSD caps the review session (3 by default).
	MaxBudgetUSD float64
	// Budgets is node.yaml's `budgets:` in force, read live (ADR 012,
	// Loop.KindModelBudget); nil falls back to Cfg's own, read once at
	// start.
	Budgets func(kind, model string) (amount float64, source string)
	// Model is the node's own evaluate model (node.yaml evaluate, else
	// default; "" the CLI's default). An item that names a model or an
	// agent with one evaluates on the strongest of this, its build model
	// and sonnet (EvaluateModel); one that names none, on this alone.
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
	// Fallback is the node's one old-platform notice (nil: every item).
	Fallback *TokenFallback
	// Codex is the Codex harness (nil: none). An evaluation runs on it
	// when one of CodexAccounts has headroom for the item's priority, or
	// the item pins it; and an item of an urgent or high goal whose first
	// verdict passes is evaluated again on the other family, with the
	// same prompt in the same workspace
	// (docs/adr/adr-005-codex-second-opinion.md,
	// docs/adr/adr-011-evaluations-run-on-the-family-with-headroom.md).
	Codex harness.Harness
	// CodexPreflight checks, before any Codex session starts, that the
	// session's job reaches the built-in context server (the Codex
	// harness's Preflight; nil: no check). A failure moves an unpinned
	// evaluation to Claude, keeps a pinned one waiting in evaluating, and
	// makes a Codex second opinion unavailable.
	CodexPreflight func(ctx context.Context, job harness.Job) error
	// CodexModel maps the Claude evaluate model to Codex's
	// (CodexConfig.CodexModel; nil: Codex's own default).
	CodexModel func(claudeModel string) string
	// CodexAccount is the Limits key of the Codex harness's own account
	// (CodexLimitsAccount of node.yaml's codex: account): a reading there
	// that paces or stops it skips a second opinion on it.
	CodexAccount string
	// CodexAccounts are the Codex accounts an evaluation may run on
	// first (Loop.CodexCandidates: the issued ones Known holds, and
	// node.yaml's codex: account, which has no ID). Nil: Codex is only
	// the second opinion, on its own account.
	CodexAccounts func() []AccountConfig
	// Waits says once per item that an item pinned to a family with no
	// headroom waits in evaluating (nil: every time).
	Waits *OncePer
	// Blind says once per item that an evaluation which saw no context
	// (toolBroken) could not run on the other family either, and the item
	// waits in evaluating (nil: every time).
	Blind *OncePer
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
	// Sandbox, when set, runs the session in its own pod with the
	// services its checkout declares (as Runner.Sandbox).
	Sandbox *sandbox.Sandbox
	Log     *log.Logger
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
	if err := e.GW.EvaluateWorkItem(ctx, item.ID, false, notes, 0, nil); err != nil {
		return fmt.Errorf("record verdict: %w", err)
	}
	return nil
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
	res, err := Resolve(ctx, e.GW, item)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
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
	spec := WorkspaceSpec{Root: e.WorkRoot, GoalSlug: res.Goal.Slug, Seq: item.Seq, Repo: res.Repo.FullName(), Branch: branch, BaseRef: res.BaseRef()}
	// the item's own GitHub token, as its builder had
	cred, err := NewItemCred(ctx, e.GW, e.WS, e.Scopes, item, spec, e.Fallback, e.logf)
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
	if !alreadyDone {
		diff, err = e.WS.Diff(ctx, dir, spec.BaseRef)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("diff: %w", err)
			}
			return e.failDiff(ctx, item, label, spec.BaseRef, branch, err)
		}
	}
	// the same context the builder had: the ADRs it was bound by
	orgLogin := e.Scopes.LoginOf(item.OrgID)
	cx, err := FetchContext(ctx, e.GW, orgLogin, res.Project.Slug)
	if err != nil {
		e.logf("item %s: context unavailable: %v", label, err)
		cx = &Context{}
	}
	AddLive(ctx, e.GW, cx, item.OrgID, res.Project.ID, e.logf)
	files, err := WriteContext(dir, cx)
	if err != nil {
		return fmt.Errorf("write context: %w", err)
	}
	contextDir := ""
	if len(files) > 0 || len(cx.Index) > 0 || len(cx.Live) > 0 {
		contextDir = ContextDir
	}
	in := harness.EvaluateInput{
		GoalSlug: res.Goal.Slug, GoalTitle: res.Goal.Title, GoalAcceptance: res.Goal.Acceptance,
		Seq: item.Seq, Title: item.Title, Brief: item.Brief, Acceptance: item.Acceptance,
		Repo: res.Repo.FullName(), Branch: branch, BaseRef: spec.BaseRef, Diff: diff,
		ContextDir: contextDir, ContextFiles: files, Project: orgLogin + "/" + res.Project.Slug, Prologue: len(e.MCP) > 0,
		AlreadyDone: alreadyDone, Evidence: item.Evidence,
	}
	model := fam.model
	budget, budgetSource := e.budgetWithSource(res.Goal, model)
	e.logf("item %s: evaluate budget $%.2f (%s)", label, budget, budgetSource)
	job := harness.Job{Kind: harness.KindEvaluate, Workspace: dir, Prompt: harness.EvaluatePrompt(in), MaxBudgetUSD: budget, Label: label + " evaluate", Timeout: e.Timeout, MCPServers: e.MCP, Model: model}
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
	sc, _ := e.Scopes.ByOrg(item.OrgID)
	ref := gateway.SessionRef{Kind: string(harness.KindEvaluate), ItemID: item.ID, GoalID: item.GoalID}
	acct, fam, err := e.openFamily(ctx, ref, sc.NodeID, fam, label)
	if err != nil {
		return err
	}
	// whichever account the session ends on (a failed preflight swaps it)
	defer unlessDetached(ctx, func() { acct.Cleanup() })
	base := job
	base.Env = slices.Clone(job.Env)
	var second harness.Job
	var st SessionTools
	runtimes := e.runtimes()
	preflight := ""
	for {
		job = base
		job.Env = slices.Clone(base.Env)
		model, job.Model = fam.model, fam.model
		job.Meta = harness.SessionMeta{NodeID: sc.NodeID, ItemID: item.ID, GoalID: item.GoalID, Attempt: item.Attempts, TempDirs: sessionTempDirs(job, acct)}
		// the second opinion's job: the same brief, none of the first
		// session's account and none of the pod's services (Codex runs on
		// the host; a Claude second opinion opens its own account)
		second = job
		second.Env = slices.Clone(job.Env)
		acct.Apply(&job)
		// the evaluator's tools, beside the built-in mesh-context. An item in
		// evaluating cannot be released (the platform releases a claimed or
		// running one): a required tool this node cannot start leaves it
		// unevaluated, and the loop does not try again while that holds.
		st = SelectTools(e.MCP, acct.Tools, string(harness.KindEvaluate), runtimes)
		if len(st.Missing) > 0 {
			e.Declines.Record(item.ID, st.Missing, runtimes, time.Now())
			return fmt.Errorf("declined, %s", st.DeclineReason())
		}
		if err := st.UseTools(&job, e.MCP, acct); err != nil {
			return fmt.Errorf("tools: %w", err)
		}
		if !fam.codex {
			break
		}
		// a Codex session that cannot reach the context server would judge
		// blind: it does not start, and a tool failure never becomes a
		// verdict (ADR 011, the preflight)
		why := e.preflight(ctx, job, acct)
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
		if acct, err = e.Credentials.Open(ctx, ref, sc.NodeID, e.Accounts, AdapterClaudeCode); err != nil {
			return err
		}
	}
	if len(st.Use) > 0 {
		e.logf("item %s: evaluate with tools %s", label, st.Names())
	}
	if note := st.OmittedNote(); note != "" {
		e.logf("item %s: %s", label, note)
	}
	// the checkout's services, started in the session's pod before the
	// evaluator; the pod and its sidecars go on every way out. An item
	// in evaluating can be neither released nor reported on: a required
	// service that does not come up leaves it unevaluated, and the loop
	// does not try again while that holds; a missing optional one is in
	// the verdict's notes.
	pod, err := OpenSessionPod(ctx, e.Sandbox, dir, job.Label, harness.KindEvaluate, e.logf)
	defer pod.Close()
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
	pod.Apply(&job)
	in.Services = pod.Prompt()
	job.Prompt = harness.EvaluatePrompt(in)
	if alreadyDone {
		e.logf("item %s: evaluate on %s: the already_done claim on %s", label, fam.logName(), spec.BaseRef)
	} else {
		e.logf("item %s: evaluate on %s: %s (%d bytes of diff)", label, fam.logName(), branch, len(diff))
	}
	h := e.Harness
	if fam.codex {
		// a Codex session still ends with the node
		h = e.Codex
	} else {
		// the session outlives this process: a restart adopts it (ADR 009)
		job.Outlive = true
	}
	result, err := h.Run(ctx, job, nil)
	// the usage report rides the verdict (afterEvaluate); Finish logs it
	// when nothing carried it
	defer acct.Finish(ctx, result)
	return e.afterEvaluate(ctx, evalRun{item: item, res: res, label: label, model: model, fam: fam, dir: dir, acct: acct, second: &second, preflight: preflight, omitted: st.OmittedNote(), missing: pod.MissingNote()}, result, err)
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
}

// afterEvaluate is what follows an evaluation session, however this
// process came to hold it: the verdict read, the second opinion asked
// for an urgent or high goal's pass, and the verdict recorded. An error
// means none was recorded. A cancelled ctx leaves the session running
// for the next node to adopt.
func (e *Evaluator) afterEvaluate(ctx context.Context, ev evalRun, result harness.Result, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	defer harness.RemoveSession(ev.dir)
	item, label, acct := ev.item, ev.label, ev.acct
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("evaluator session ran out of time (%s)", e.Timeout)
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
	fam, head := ev.fam, ""
	var passed bool
	var notes string
	cost := result.CostUSD
	if blind, broken := e.toolBroken(result, acct, ev.second == nil); broken {
		other := ev.fam.other(e)
		saw := fmt.Sprintf("item %s: evaluation on %s saw no context (%s); not a verdict", label, ev.fam.fullName(), blind)
		if why, ok := e.canRetry(ev, other); !ok {
			return e.blind(item.ID, fmt.Sprintf("%s, and %s cannot evaluate it: %s", saw, other.name(), why))
		}
		e.logf("%s, evaluating on %s", saw, other.fullName())
		op := e.secondOpinion(ctx, ev, other, *ev.second, "evaluation")
		defer op.acct.Finish(ctx, op.result)
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
			return e.blind(item.ID, fmt.Sprintf("%s, and the evaluation on %s is unavailable too: %s", saw, other.fullName(), op.unavailable))
		}
		e.Blind.Forget(item.ID)
		head = fmt.Sprintf("%s (first attempt, %s): no verdict — %s", ev.fam.name(), ev.fam.shortModel(), blind)
		fam, passed, notes = other, op.passed, op.notes
		cost += op.cost
	} else {
		var ok bool
		passed, notes, ok = harness.ParseVerdict(result.Text)
		if !ok {
			return fmt.Errorf("evaluator session reached no verdict (%s): %s%s", result.SessionID, truncate(result.Text, 300), keptAt(result))
		}
		notes = strings.TrimSpace(notes)
		if notes == "" {
			notes = "(no notes)"
		}
	}
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
	// the item's events say which family and model judged it
	evaluatedOn := fam.logName()
	if ev.preflight != "" && head == "" {
		// and why not on Codex, which the item might have expected
		evaluatedOn = fam.fullName() + "; the Codex preflight failed: " + truncate(ev.preflight, 200)
	}
	if passed && (fam.codex || e.hasCodex()) && gateway.PriorityRank(ev.res.Goal.Priority) <= gateway.PriorityRank("high") {
		// the second opinion is the family that gave no verdict of its own
		other := fam.other(e)
		if head != "" {
			other = ev.fam
		}
		op := opinion{unavailable: "the evaluation was adopted after a restart, and its prompt is not kept"}
		switch {
		case ev.preflight != "" && other.codex:
			// the preflight that moved it to Claude has just failed
			op = opinion{unavailable: "preflight " + ev.preflight}
		case ev.second != nil:
			op = e.secondOpinion(ctx, ev, other, *ev.second, "second opinion")
		}
		defer op.acct.Finish(ctx, op.result)
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
			notes = truncate(notes, room-200-len(line)-len(without)-len(ev.preflight)) + "\n\n" + line + "\n\n(evaluated on " + evaluatedOn + ")" + without
		} else {
			passed = passed && op.passed
			notes = bothNotes(fam, notes, other, op.notes, op.passed, room-200-len(without)-len(ev.preflight)) + "\n\n(evaluated on " + evaluatedOn + "; second opinion on " + other.fullName() + ")" + without
		}
	} else {
		notes = truncate(notes, room-100-len(without)-len(ev.preflight)) + "\n\n(evaluated on " + evaluatedOn + ")" + without
	}
	if head != "" {
		notes = head + "\n\n" + notes
	}
	verdict := "failed"
	if passed {
		verdict = "passed"
	}
	if err := acct.Carried(e.GW.EvaluateWorkItem(ctx, item.ID, passed, notes, Cents(cost), usage)); err != nil {
		return fmt.Errorf("record verdict: %w", err)
	}
	e.logf("item %s: evaluation %s on %s ($%.2f)", label, verdict, fam.fullName(), cost)
	return nil
}

// AdoptEvaluation takes up an evaluation session a node process before
// this one started (ADR 009): followed to its end, or read from its
// file when it is gone, and its verdict recorded by afterEvaluate. An
// item no longer in evaluating on this node is not its to judge: the
// session is stopped and its directory removed.
func (e *Evaluator) AdoptEvaluation(ctx context.Context, item gateway.WorkItem, s FoundSession) error {
	if item.Status != "evaluating" || item.NodeID != s.State.NodeID {
		e.logf("item %s: adopted evaluation is no longer this node's (%s); stopped", item.ID, item.Status)
		s.Stop()
		return harness.RemoveSession(s.State.Workspace)
	}
	res, err := Resolve(ctx, e.GW, item)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	at, ok := e.Harness.(harness.Attacher)
	if !ok {
		s.Stop()
		harness.RemoveSession(s.State.Workspace)
		return errNoAttach
	}
	label := fmt.Sprintf("%s/%d", res.Goal.Slug, item.Seq)
	e.logf("item %s: adopted its %s evaluation (pid %d)", label, s.Liveness(), s.State.PID)
	result, err := at.Attach(ctx, s.Dir, nil)
	return e.afterEvaluate(ctx, evalRun{item: item, res: res, label: label, model: s.State.Model, fam: family{model: s.State.Model, claude: s.State.Model}, dir: s.State.Workspace}, result, err)
}

// opinion is the second evaluation's outcome: its verdict and notes, or
// why there is none (unavailable), and what it cost either way.
type opinion struct {
	passed      bool
	notes       string
	cost        float64
	unavailable string
	// result is the session's, for its tokens in the evaluation's report
	result harness.Result
	// acct is a Claude second opinion's own account, whose report the
	// evaluation's carries (nil: a Codex one, on the harness's account)
	acct *SessionAccount
}

// secondOpinion runs the evaluation's job on the other family (on):
// Codex after Claude, unless its account is paced or stopped; Claude
// after Codex, on an account of its own. A session that errors, times
// out, reaches no verdict or saw no context (toolBroken) is
// unavailable, and the first verdict stands. role names it in the job's
// label and node.log: "second opinion", or "evaluation" for the one
// that stands in for a session that saw no context.
func (e *Evaluator) secondOpinion(ctx context.Context, ev evalRun, on family, job harness.Job, role string) opinion {
	label := ev.label
	h := e.Harness
	var acct *SessionAccount
	sc, _ := e.Scopes.ByOrg(ev.item.OrgID)
	ref := gateway.SessionRef{Kind: string(harness.KindEvaluate), ItemID: ev.item.ID, GoalID: ev.item.GoalID}
	var open func() (*SessionAccount, error)
	if on.codex {
		a, why, ok := e.codexForSecond(ev.res.Goal.Priority)
		if !ok {
			return opinion{unavailable: why}
		}
		h = e.Codex
		if a.ID != "" {
			// an issued Codex account: opened as a first Codex
			// evaluation's is; the harness's own needs nothing opened
			open = func() (*SessionAccount, error) {
				return e.Credentials.Open(withChooser(ctx, e.codexChooser(ev.res.Goal.Priority, false)), ref, sc.NodeID, nil, AdapterCodex)
			}
		}
	} else {
		open = func() (*SessionAccount, error) {
			return e.Credentials.Open(ctx, ref, sc.NodeID, e.Accounts, AdapterClaudeCode)
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
	op := opinion{cost: result.CostUSD, result: result, acct: acct}
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
	default:
		if blind, broken := e.toolBroken(result, acct, false); broken {
			op.unavailable = "saw no context (" + harness.Redact(blind, job.Secrets...) + ")"
			break
		}
		passed, notes, ok := harness.ParseVerdict(acct.Redact(result.Text))
		if !ok {
			op.unavailable = "no verdict: " + truncate(acct.Redact(harness.Redact(result.Text, job.Secrets...)), 300)
			break
		}
		op.passed = passed
		op.notes = strings.TrimSpace(harness.Redact(notes, job.Secrets...))
		if op.notes == "" {
			op.notes = "(no notes)"
		}
	}
	return op
}

// ErrNoContext is an evaluation that saw no context — its built-in
// context server, or a required console tool, never answered
// (toolBroken) — and could not be evaluated on the other family either:
// nothing is recorded, and the item waits in evaluating for a later tick
// (ADR 011, a tool failure is never a verdict).
var ErrNoContext = errors.New("the evaluation saw no context, and the other family could not evaluate it")

// toolBroken names the tool a session could not use and its first
// error, capped at 200 characters: a built-in server of the evaluator's
// (MCP: the context server) or a required console tool the session was
// served, every one of whose calls errored, or which errored with no
// call at all (not connected at startup). A session with one clean call
// on each is not broken. adopted is a session an older node process
// started (AdoptEvaluation): only there is prologue the built-in.
func (e *Evaluator) toolBroken(res harness.Result, acct *SessionAccount, adopted bool) (string, bool) {
	broken := func(name string) (string, bool) {
		s := res.Tools[name]
		if s == nil || s.Errors == 0 || s.Errors < s.Calls {
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
	// under prologue reports that name, not tools.Builtin
	// (tools.IsBuiltin, an alias read until 2026-10-28). A session this
	// process started was rendered mesh-context, so a prologue in its
	// result is a console tool: broken only if required, below.
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

// codexForSecond is the Codex account a second opinion for a goal of
// that priority runs on, or why there is none: none of CodexAccounts
// has headroom, or — with no CodexAccounts — the harness's own account
// (CodexAccount) is paced or stopped.
func (e *Evaluator) codexForSecond(priority string) (AccountConfig, string, bool) {
	if e.CodexAccounts != nil {
		a, ok := e.chooseCodex(priority, false)
		if !ok {
			return AccountConfig{}, "no Codex account has headroom", false
		}
		return a, "", true
	}
	if e.Limits != nil && e.CodexAccount != "" {
		if r, ok := e.Limits.Reading(e.CodexAccount); ok {
			if p := pace(time.Now(), r, e.thresholds()); p.mode != paceNormal {
				return AccountConfig{}, fmt.Sprintf("account %s is %s until %s: %s", e.CodexAccount, p.mode, p.resumeAt.UTC().Format(time.RFC3339), p.why), false
			}
		}
	}
	return AccountConfig{}, "", true
}

// bothNotes labels the two verdicts, the first family's first, each by
// its family and model, so a builder sees both, truncated together to
// n: each gets half, and one shorter than its half leaves the rest to
// the other, so neither is cut out.
func bothNotes(firstOn family, first string, secondOn family, second string, secondPassed bool, n int) string {
	firstHead := firstOn.name() + " (first evaluation, " + firstOn.shortModel() + "): " + harness.VerdictPass + "\n"
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
	return firstHead + truncate(first, a) + "\n\n" + secondHead + truncate(second, b)
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
	// account is a Codex session's: an issued one (its ID set) or the
	// harness's own (node.yaml's codex:, no ID)
	account  AccountConfig
	priority string
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
		a, ok := e.chooseCodex(priority, true)
		if !ok {
			return fam, e.waits(item.ID, label, model)
		}
		return family{codex: true, pinned: true, model: model, claude: claude, account: a, priority: priority}, nil
	}
	if a, ok := e.chooseCodex(priority, false); ok {
		return family{codex: true, model: e.codexModel(claude), claude: claude, account: a, priority: priority}, nil
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
// claim-time read issues) or on the harness's own; a Claude one as
// ever. An unpinned Codex evaluation none of whose issued credentials
// has headroom after all runs on Claude instead; a pinned one waits.
func (e *Evaluator) openFamily(ctx context.Context, ref gateway.SessionRef, nodeID string, fam family, label string) (*SessionAccount, family, error) {
	if fam.codex {
		var acct *SessionAccount
		var err error
		if fam.account.ID == "" {
			acct, err = e.Credentials.OpenOwn(ctx, ref, nodeID, fam.account)
		} else {
			acct, err = e.Credentials.Open(withChooser(ctx, e.codexChooser(fam.priority, fam.pinned)), ref, nodeID, nil, AdapterCodex)
		}
		switch {
		case err == nil:
			e.Waits.Forget(ref.ItemID)
			acct.Model = fam.model
			return acct, fam, nil
		case !errors.Is(err, ErrNoAccount):
			return nil, fam, err
		case fam.pinned:
			return nil, fam, e.waits(ref.ItemID, label, fam.model)
		}
		e.logf("item %s: no issued Codex credential has headroom after all; evaluating on Claude", label)
		fam = family{model: fam.claude, claude: fam.claude, priority: fam.priority}
	}
	acct, err := e.Credentials.Open(ctx, ref, nodeID, e.Accounts, AdapterClaudeCode)
	return acct, fam, err
}

// CodexHeadroom says whether a Codex account has headroom for an
// unpinned evaluation of that priority (chooseCodex).
func (e *Evaluator) CodexHeadroom(priority string) bool {
	_, ok := e.chooseCodex(priority, false)
	return ok
}

// hasCodex says the evaluator has Codex to run on at all: its harness,
// and an account — any of CodexAccounts, or (nil CodexAccounts) the
// harness's own.
func (e *Evaluator) hasCodex() bool {
	return e.Codex != nil && (e.CodexAccounts == nil || len(e.CodexAccounts()) > 0)
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
		subs := slices.DeleteFunc(slices.Clone(accounts), func(a AccountConfig) bool { return a.Kind == AccountAPI })
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
