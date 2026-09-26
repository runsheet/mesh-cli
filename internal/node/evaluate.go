package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// Evaluator is the second fresh session an item gets on the node
// (mesh-specs trd §3.4): the finished branch checked out, its diff from
// the base, the acceptance, an adversarial review — and its verdict
// recorded with EvaluateWorkItem. The platform opens the pull request on
// a pass and readies the item again on a fail (with the notes for the
// next builder), so the evaluator changes nothing itself.
type Evaluator struct {
	GW      Gateway
	Harness harness.Harness
	// Availability is the node's harness condition, shared with the loop:
	// a session ending on the harness's out-of-credit or rate-limit
	// message, or the second silent one running, trips it (trd §3); nil
	// notes nothing.
	Availability *Availability
	WS           Workspacer
	Scopes       Scopes
	WorkRoot     string
	// MCP are the MCP servers the session gets (Prologue).
	MCP map[string]any
	// MaxBudgetUSD caps the review session (3 by default).
	MaxBudgetUSD float64
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
	// Second is the second opinion's harness (Codex; nil: none). An
	// item of an urgent or high goal whose first verdict passes is
	// evaluated again on it, with the same prompt in the same workspace
	// (docs/adr/adr-005-codex-second-opinion.md).
	Second harness.Harness
	// SecondModel is the second opinion's model (CodexModel of the
	// evaluate model; "" Codex's own default).
	SecondModel string
	// SecondAccount is the Limits key of Second's account
	// (CodexLimitsAccount): a reading there that paces or stops it skips
	// the second opinion, and nothing else.
	SecondAccount string
	// Limits and Thresholds judge SecondAccount (nil: never skipped).
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
	if err := e.GW.EvaluateWorkItem(ctx, item.ID, false, notes, 0); err != nil {
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
	branch := item.Branch
	if branch == "" {
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
	diff, err := e.WS.Diff(ctx, dir, spec.BaseRef)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("diff: %w", err)
		}
		return e.failDiff(ctx, item, label, spec.BaseRef, branch, err)
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
	}
	budget := e.MaxBudgetUSD
	if budget <= 0 {
		budget = 3
	}
	model := EvaluateModel(ctx, e.GW, item, e.Model)
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
	acct, err := e.Credentials.Open(ctx, gateway.SessionRef{Kind: string(harness.KindEvaluate), ItemID: item.ID, GoalID: item.GoalID}, sc.NodeID, e.Accounts)
	if err != nil {
		return err
	}
	defer unlessDetached(ctx, acct.Cleanup)
	job.Meta = harness.SessionMeta{NodeID: sc.NodeID, ItemID: item.ID, GoalID: item.GoalID, Attempt: item.Attempts, TempDirs: sessionTempDirs(job, acct)}
	// the second opinion's job: the same brief, none of the Claude account
	// and none of the pod's services (Codex runs on the host)
	second := job
	acct.Apply(&job)
	// the evaluator's tools, beside the built-in Prologue. An item in
	// evaluating cannot be released (the platform releases a claimed or
	// running one): a required tool this node cannot start leaves it
	// unevaluated, and the loop does not try again while that holds.
	runtimes := e.runtimes()
	st := SelectTools(e.MCP, acct.Tools, string(harness.KindEvaluate), runtimes)
	if len(st.Missing) > 0 {
		e.Declines.Record(item.ID, st.Missing, runtimes, time.Now())
		return fmt.Errorf("declined, %s", st.DeclineReason())
	}
	if err := st.UseTools(&job, e.MCP, acct); err != nil {
		return fmt.Errorf("tools: %w", err)
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
	e.logf("item %s: evaluate on %s: %s (%d bytes of diff)", label, modelName(model), branch, len(diff))
	// the session outlives this process: a restart adopts it (ADR 009)
	job.Outlive = true
	result, err := e.Harness.Run(ctx, job, nil)
	acct.Finish(ctx, result)
	return e.afterEvaluate(ctx, evalRun{item: item, res: res, label: label, model: model, dir: dir, acct: acct, second: &second, omitted: st.OmittedNote(), missing: pod.MissingNote()}, result, err)
}

// evalRun is one evaluation session as its tail needs it, whether this
// process started it (Evaluate) or adopted it (AdoptEvaluation).
type evalRun struct {
	item  gateway.WorkItem
	res   *Resolved
	label string
	model string
	dir   string
	acct  *SessionAccount // nil for an adopted session
	// second is the second opinion's job; nil for an adopted session,
	// whose prompt is not kept
	second           *harness.Job
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
	if tripped, why := e.Availability.Observe(result, err); tripped {
		return fmt.Errorf("%w: %s", ErrHarnessUnavailable, acct.Redact(why))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("evaluator session ran out of time (%s)", e.Timeout)
	}
	if err != nil {
		return fmt.Errorf("evaluator session: %w", acct.RedactErr(err))
	}
	result.Text = acct.Redact(result.Text)
	passed, notes, ok := harness.ParseVerdict(result.Text)
	if !ok {
		return fmt.Errorf("evaluator session reached no verdict (%s): %s", result.SessionID, truncate(result.Text, 300))
	}
	notes = strings.TrimSpace(notes)
	if notes == "" {
		notes = "(no notes)"
	}
	// the notes say which tools and services the evaluator ran without
	without := ""
	if ev.omitted != "" {
		without = "\n" + truncate(ev.omitted, 500)
	}
	if ev.missing != "" {
		without += "\n" + truncate(ev.missing, 500)
	}
	cost := result.CostUSD
	if passed && e.Second != nil && gateway.PriorityRank(ev.res.Goal.Priority) <= gateway.PriorityRank("high") {
		op := opinion{unavailable: "the evaluation was adopted after a restart, and its prompt is not kept"}
		if ev.second != nil {
			op = e.secondOpinion(ctx, label, *ev.second)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cost += op.cost
		if op.unavailable != "" {
			e.logf("item %s: Codex second opinion unavailable: %s", label, op.unavailable)
			line := "Codex second opinion unavailable: " + op.unavailable
			notes = truncate(notes, 4000-200-len(line)-len(without)) + "\n\n" + line + "\n\n(evaluated on " + modelName(ev.model) + ")" + without
		} else {
			passed = passed && op.passed
			notes = bothNotes(notes, op.notes, op.passed, 4000-200-len(without)) + "\n\n(evaluated on " + modelName(ev.model) + "; second opinion on Codex" + codexModelName(e.SecondModel) + ")" + without
		}
	} else {
		// the item's events say which model judged it
		notes = truncate(notes, 4000-100-len(without)) + "\n\n(evaluated on " + modelName(ev.model) + ")" + without
	}
	verdict := "failed"
	if passed {
		verdict = "passed"
	}
	if err := e.GW.EvaluateWorkItem(ctx, item.ID, passed, notes, Cents(cost)); err != nil {
		return fmt.Errorf("record verdict: %w", err)
	}
	e.logf("item %s: evaluation %s ($%.2f)", label, verdict, cost)
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
	return e.afterEvaluate(ctx, evalRun{item: item, res: res, label: label, model: s.State.Model, dir: s.State.Workspace}, result, err)
}

// opinion is the second evaluation's outcome: its verdict and notes, or
// why there is none (unavailable), and what it cost either way.
type opinion struct {
	passed      bool
	notes       string
	cost        float64
	unavailable string
}

// secondOpinion runs the evaluation's job on Second, unless its account
// is paced or stopped. A session that errors, times out or reaches no
// verdict is unavailable, and the first verdict stands.
func (e *Evaluator) secondOpinion(ctx context.Context, label string, job harness.Job) opinion {
	if e.Limits != nil && e.SecondAccount != "" {
		if r, ok := e.Limits.Reading(e.SecondAccount); ok {
			var cfg LimitsConfig
			if e.Thresholds != nil {
				cfg = e.Thresholds()
			}
			if p := pace(time.Now(), r, cfg); p.mode != paceNormal {
				return opinion{unavailable: fmt.Sprintf("account %s is %s until %s: %s", e.SecondAccount, p.mode, p.resumeAt.UTC().Format(time.RFC3339), p.why)}
			}
		}
	}
	job.Model = e.SecondModel
	job.Label = label + " second opinion"
	e.logf("item %s: second opinion on Codex%s", label, codexModelName(e.SecondModel))
	result, err := e.Second.Run(ctx, job, nil)
	op := opinion{cost: result.CostUSD}
	switch {
	case ctx.Err() != nil:
		op.unavailable = ctx.Err().Error()
	case errors.Is(err, context.DeadlineExceeded):
		op.unavailable = fmt.Sprintf("the session ran out of time (%s)", e.Timeout)
	case err != nil:
		op.unavailable = truncate(harness.Redact(err.Error(), job.Secrets...), 300)
	default:
		passed, notes, ok := harness.ParseVerdict(result.Text)
		if !ok {
			op.unavailable = "no verdict: " + truncate(harness.Redact(result.Text, job.Secrets...), 300)
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

// bothNotes labels the two verdicts, Claude's first, so a builder sees
// both, truncated together to n: each gets half, and one shorter than
// its half leaves the rest to the other, so neither is cut out.
func bothNotes(first, second string, secondPassed bool, n int) string {
	firstHead := "Claude (first evaluation): " + harness.VerdictPass + "\n"
	secondHead := "Codex (second opinion): " + verdictLine(secondPassed) + "\n"
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

// Cents rounds a dollar cost to whole cents.
func Cents(usd float64) int {
	if usd <= 0 {
		return 0
	}
	return int(math.Round(usd * 100))
}
