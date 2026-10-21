package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
	"projectmesh.io/mesh-cli/internal/validate"
)

// validation.go is a build's validation (runsheet ADR 032, goal
// node-attests-its-validation): after a session commits and before the
// node pushes, the repository's .runsheet/validate.yml runs on the
// commit the item finishes with — the suites the branch's diff affects
// (validate.Select: no paths is every change), none included, plus every
// `always` one — in the session's pod when the node has a
// sandbox, else on the host, saying so. The attestation it produces is
// written beside the session and uploaded on the item's token after the
// push; a failing step gives the builder one more turn, then fails the
// item naming the step. Each step's time rides the usage record.

// AttestationFile is the attestation's name in the session directory
// (<workdir>.session/attestation.json).
const AttestationFile = "attestation.json"

// buildValidation is one build's validation: where its steps run, who
// ran them, and every run's attestation (the first, and the retry's).
type buildValidation struct {
	runner validate.Runner
	meta   validate.Meta
	close  func()
	runs   []validate.Attestation
	// baseGoneSaid: the build's base was found gone and said so, once
	// for every run of the build's validation
	baseGoneSaid bool
}

// validationFailure is a step that failed on the commit the build
// would finish with: the item's failure, naming the step first.
type validationFailure struct {
	Step validate.StepResult
}

func (e *validationFailure) Error() string {
	base := fmt.Sprintf("validation failed: step %s exited %d", e.Step.Name, e.Step.ExitCode)
	if e.Step.EnvironmentKind != validate.ToolMissing {
		return base
	}
	if tool := validate.MissingTool(e.Step.Output); tool != "" {
		return fmt.Sprintf("%s: command not found: %s (in `%s`)", base, tool, e.Step.Command)
	}
	return fmt.Sprintf("%s: `%s`", base, e.Step.Command)
}

// EnvironmentKind is the failing step's (docs/adr/adr-030): a timeout or
// a missing tool is the node's, not the code's; a code failure none.
func (e *validationFailure) EnvironmentKind() validate.EnvironmentKind {
	return e.Step.EnvironmentKind
}

// validator runs a checkout's validation and uploads its attestation
// for whichever session it follows: a build's (Runner.validator) or a
// rebase's (Rebaser.validator), each on the node's own gateway.
type validator struct {
	GW      Gateway
	WS      Workspacer
	Scopes  Scopes
	Sandbox *sandbox.Sandbox
	Inspect sandbox.Runner
	Notices *ValidationNotices
	logf    func(format string, args ...any)
	// WorkRoot is the node's work root, under which a host run's
	// scratch is made (ScratchRoot, scratch.go); "" is ~/.mesh/work.
	WorkRoot string
	// Layout, when set, puts a host run's scratch in its item's job and
	// its pod on its connection's store (ADR-039).
	Layout *Layout
	// BuildCache is a run's GOCACHE; "" is CacheRoot's go/.
	BuildCache string
	// CacheRoot is the shared cache's root (the Runner's Cache.Root),
	// whose blobs/ a sandboxed run mounts read-only; "" is
	// SharedCacheRoot().
	CacheRoot string
}

// validationTarget is what a validation needs of the session it
// follows: the item and its checkout, the base its diff is taken
// against, the log prefix ("item <label>", "rebase <label>"), the
// session's job and pod (nil when adopted), its id, how its errors are
// redacted and how a note reaches the item.
type validationTarget struct {
	item    gateway.WorkItem
	dir     string
	base    string
	prefix  string
	label   string
	job     *harness.Job
	pod     *sandbox.Session
	session func() string
	redact  func(error) error
	report  func(ctx context.Context, note string)
	// cred is the item's token — or a merge batch candidate's
	// (NewBatchCred) — every fetch the validator runs going out on it
	// (ForFetch, OnItemToken); nil on the fallback.
	cred *ItemCred
	// batchID is the merge batch a candidate's validation attests
	// (mainvalidation.go): set, there is no item — the upload goes by
	// RecordBatchCandidateAttestation carrying it. repoTok, set with no
	// cred, is a token its fetches go out on as it is, never renewed.
	batchID string
	repoTok *gateway.GitHubToken
	// defaultBranch is the repo's default branch, the changed paths'
	// base when base — a merged dependency's branch — is gone from
	// origin; "" takes no fallback.
	defaultBranch string
	// tokenName, set, is whose token cred is (`merge batch 7`): every
	// fetch goes out on a fresh token or not at all, a renewal refused
	// being the validation's error naming it (*fetchTokenError), never
	// a fetch on the token held (fetchContext).
	tokenName string
	// diffFrom, set, is the commit the changed paths are taken from in
	// base's place: a merged item's merge commit is diffed against its
	// first parent, since main already holds it (mainvalidation.go).
	diffFrom string
	// served, set, is a commit materialised from GitHub's served diff
	// at dir (serveddiff.go): no git there, so its changed paths are the
	// compare's, its head and tree the ones read and checked.
	served *servedTree
	// spec, set, is the checkout's workspace: a run that fails on its
	// worktree's gone admin dir rebuilds it by spec and runs once more
	// (rebuildAndRetry); unset, there is no retry.
	spec WorkspaceSpec
}

func (r *Runner) validator() validator {
	vr := validator{GW: r.GW, WS: r.WS, Scopes: r.Scopes, Sandbox: r.Sandbox, Inspect: r.Inspect, Notices: r.Notices, logf: r.logf, WorkRoot: r.WorkRoot, Layout: r.Layout, BuildCache: r.BuildCache}
	if r.Cache != nil {
		vr.CacheRoot = r.Cache.Root
	}
	return vr
}

// cacheDirs are a run's Go build cache and the shared cache's blob
// store: BuildCache, else go/ under the cache root, and blobs/ under it.
func (vr validator) cacheDirs() (goCache, blobs string, err error) {
	root := vr.CacheRoot
	if root == "" {
		if root, err = SharedCacheRoot(); err != nil {
			return "", "", err
		}
	}
	goCache = vr.BuildCache
	if goCache == "" {
		goCache = filepath.Join(root, "go")
	}
	return goCache, filepath.Join(root, "blobs"), nil
}

// removeScratch removes a run's scratch dir: on the host as
// RemoveScratch does, and with a sandbox as a pod's HOME is removed
// (Sandbox.RemoveTree), so files a step left there owned by the pod's
// mapped user go too.
func (vr validator) removeScratch(dir string) error {
	if vr.Sandbox == nil {
		return RemoveScratch(dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return vr.Sandbox.RemoveTree(ctx, dir)
}

// target is the build as its validation needs it.
func (b *buildRun) target() validationTarget {
	return validationTarget{item: b.item, dir: b.dir, base: b.spec.BaseRef, defaultBranch: defaultBranchOf(b.res), prefix: "item " + b.label, label: b.label,
		job: b.job, pod: b.podSession(), session: b.session, redact: b.acct.RedactErr, report: func(ctx context.Context, note string) { b.report(ctx, note) }, cred: b.cred, spec: b.spec}
}

// defaultBranchOf is res's repo's default branch, "main" when unread,
// as Resolved.BaseRef falls back.
func defaultBranchOf(res *Resolved) string {
	if res != nil && res.Repo != nil && res.Repo.DefaultBranch != "" {
		return res.Repo.DefaultBranch
	}
	return "main"
}

// startValidation reads the checkout's validate.yml and readies the
// runner its steps go through: nil when the repository declares none
// (said once per item), the finish as before.
func (r *Runner) startValidation(ctx context.Context, b *buildRun) (*buildValidation, error) {
	return r.validator().start(ctx, b.target())
}

func (vr validator) start(ctx context.Context, t validationTarget) (*buildValidation, error) {
	if _, err := validate.Load(t.dir); errors.Is(err, validate.ErrNoValidation) {
		vr.logf("%s: no validation declared", t.prefix)
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	sc, _ := vr.Scopes.ByOrg(t.item.OrgID)
	v := &buildValidation{meta: validate.Meta{NodeID: sc.NodeID, ItemID: t.item.ID}, close: func() {}}
	// the run's own scratch, removed by close on every path, and the
	// node's one build cache (scratch.go): nothing of it left in /tmp —
	// a sandboxed run's mounted into its pods with the shared cache's
	// blob store read-only (sandbox.ValidationCache)
	root, err := scratchRootFor(vr.Layout, validationJobID(t), vr.WorkRoot)
	if err != nil {
		return nil, fmt.Errorf("scratch: %w", err)
	}
	cache, blobs, err := vr.cacheDirs()
	if err != nil {
		return nil, fmt.Errorf("build cache: %w", err)
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, fmt.Errorf("build cache: %w", err)
	}
	subject := t.item.ID
	if t.batchID != "" {
		subject = t.batchID
	}
	scratch, err := MakeScratch(root, "validate", subject)
	if err != nil {
		return nil, err
	}
	// removes the run's scratch and, a served checkout being node-owned
	// scratch too, the checkout: after the pod, whatever a step left
	removeScratch := func() {
		dirs := []string{scratch}
		if t.served != nil {
			dirs = append(dirs, t.dir)
		}
		for _, d := range dirs {
			if err := vr.removeScratch(d); err != nil {
				vr.logf("%s: validation: remove scratch %s: %v", t.prefix, d, err)
			}
		}
	}
	if vr.Sandbox == nil {
		v.close = removeScratch
		host, _ := os.Hostname()
		v.runner = validate.HostRunner{Dir: t.dir, Env: hostValidationEnv(t.job, scratch, cache)}
		v.meta.RunnerImage, v.meta.Sandboxed = "host:"+host, false
		return v, nil
	}
	sess := t.pod
	if sess == nil {
		// an adopted session's pod ended with the node before it
		var git sandbox.GitIdentity
		if t.job != nil {
			git = sandbox.GitIdentityFromEnv(t.job.Env)
		}
		s, err := confineTo(vr.Sandbox, t.dir).Open(t.label, "validate", git)
		if err != nil {
			removeScratch()
			return nil, err
		}
		sess = s
		v.close = func() {
			if err := s.Close(); err != nil {
				vr.logf("%s", sandbox.CloseLine(s.Label, err))
			}
			removeScratch()
		}
	} else {
		v.close = removeScratch
	}
	var env []string
	if t.job != nil {
		env = t.job.Env
	}
	v.runner = sandboxRunner{sess: sess, dir: t.dir, env: env, cache: &sandbox.ValidationCache{Scratch: scratch, GoCache: cache, Blobs: blobs}}
	image := vr.Sandbox.Config.Image
	run := vr.Inspect
	if run == nil {
		run = sandbox.ExecRunner
	}
	v.meta.RunnerImage, v.meta.Sandboxed = image, true
	if digest, err := sandbox.ImageDigest(ctx, run, image); err != nil {
		vr.logf("%s: validation: %v; the attestation names the image by its ref", t.prefix, err)
	} else {
		v.meta.RunnerImage = digest
	}
	return v, nil
}

// validateBuild runs the validation on the checkout's HEAD and, when a
// step fails, gives the session one more turn (retryTurn) and runs it
// again on the new HEAD. It returns the last run's attestation and nil,
// a *validationFailure — the step still failing, or a retry that landed
// no new commit — or the run's own error. first is the build session's
// result, whose usage report the retry's is added to. The validation's
// time counts against the job's timeout.
func (r *Runner) validateBuild(ctx context.Context, b *buildRun, v *buildValidation, first harness.Result) (validate.Attestation, error) {
	if b.job != nil && b.job.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, b.started.Add(b.job.Timeout))
		defer cancel()
	}
	att, err := r.runValidation(ctx, b, v)
	if err != nil {
		return att, err
	}
	failed, ok := failedStep(att)
	if !ok {
		return att, nil
	}
	vf := &validationFailure{Step: failed}
	b.rec.check(failed)
	r.logf("item %s: %v; one more turn", b.label, vf)
	b.report(ctx, vf.Error()+"; the builder gets one more turn")
	res, runErr, ran := r.retryTurn(ctx, b, failed, att.Commit)
	if !ran {
		return att, vf
	}
	if why := b.watch.lost(); why != "" {
		// the retry turn's claim was lost: stopped, and nothing sent
		_ = r.afterLost(b, why, res)
		return att, ErrClaimLost
	}
	b.acct.Report(first)
	b.acct.Add(res)
	if ctx.Err() != nil {
		return att, ctx.Err()
	}
	if runErr != nil {
		b.report(ctx, "validation retry: harness error: "+b.acct.RedactErr(runErr).Error())
	}
	head, err := gitHead(ctx, b.dir)
	if err != nil {
		return att, err
	}
	if head == att.Commit {
		r.logf("item %s: the retry turn landed no new commit", b.label)
		b.report(ctx, "validation retry: no new commit")
		return att, vf
	}
	att, err = r.runValidation(ctx, b, v)
	if err != nil {
		return att, err
	}
	if failed, ok := failedStep(att); ok {
		b.rec.check(failed)
		return att, &validationFailure{Step: failed}
	}
	return att, nil
}

// runValidation runs the steps the branch's diff selects on HEAD and
// builds, keeps and writes that run's attestation — when it selects none
// too, an attestation of no commands, so the verify step finds one for
// the head and never answers missing for "nothing affected" (goal
// the-nodes-attestation-reaches-the-platform, rule d).
func (r *Runner) runValidation(ctx context.Context, b *buildRun, v *buildValidation) (validate.Attestation, error) {
	return r.validator().run(ctx, b.target(), v)
}

func (vr validator) run(ctx context.Context, t validationTarget, v *buildValidation) (validate.Attestation, error) {
	if t.spec.Dir() != t.dir {
		return vr.runOnce(ctx, t, v)
	}
	// the changed paths, the head or the attestation's `git rev-parse
	// <sha>^{tree}` failing on a worktree whose admin dir is gone: rebuilt
	// once and the run tried once more (ADR-028, Rebuilding a worktree)
	return rebuildAndRetry(ctx, vr.WS, t.spec, func(retry bool) (validate.Attestation, error) {
		if retry {
			if err := t.cred.Reauthorize(ctx); err != nil {
				return validate.Attestation{}, err
			}
		}
		return vr.runOnce(ctx, t, v)
	})
}

func (vr validator) runOnce(ctx context.Context, t validationTarget, v *buildValidation) (validate.Attestation, error) {
	cfg, err := validate.Load(t.dir)
	if err != nil {
		return validate.Attestation{}, err
	}
	if t.served != nil {
		return vr.runServed(ctx, t, v, cfg)
	}
	changed, from, err := vr.changedPaths(ctx, t)
	var te *fetchTokenError
	if errors.As(err, &te) {
		return validate.Attestation{}, err
	}
	if err != nil {
		// a base fetch GitHub refused is credential_refused (adr-030)
		return validate.Attestation{}, validate.Classify(fmt.Errorf("changed paths: %w", err))
	}
	if from != t.base && !v.baseGoneSaid {
		v.baseGoneSaid = true
		note := fmt.Sprintf("validation: base %s is gone (merged); changed paths from origin/%s", t.base, from)
		vr.logf("%s: %s", t.prefix, note)
		if t.report != nil {
			t.report(ctx, note)
		}
	}
	head, err := gitHead(ctx, t.dir)
	if err != nil {
		return validate.Attestation{}, err
	}
	return vr.runSteps(ctx, t, v, cfg, changed, head, func(results []validate.StepResult, meta validate.Meta) (validate.Attestation, error) {
		return validate.Build(t.dir, head, results, meta)
	})
}

// runServed is run on a served checkout: the compare's paths select the
// suites, and the attestation names the head and tree read from GitHub.
func (vr validator) runServed(ctx context.Context, t validationTarget, v *buildValidation, cfg validate.Config) (validate.Attestation, error) {
	s := t.served
	return vr.runSteps(ctx, t, v, cfg, s.changed, s.head, func(results []validate.StepResult, meta validate.Meta) (validate.Attestation, error) {
		return validate.BuildWithTree(t.dir, s.head, s.tree, results, meta)
	})
}

// runSteps runs the suites changed selects on head and builds, keeps
// and writes the run's attestation by build.
func (vr validator) runSteps(ctx context.Context, t validationTarget, v *buildValidation, cfg validate.Config, changed []string, head string,
	build func([]validate.StepResult, validate.Meta) (validate.Attestation, error)) (validate.Attestation, error) {
	steps := validate.Select(cfg, changed)
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	ran := strings.Join(names, ", ")
	if ran == "" {
		ran = "no suite affected"
	}
	vr.logf("%s: validation on %s: %s", t.prefix, short(head), ran)
	results, err := validate.Run(ctx, steps, v.runner, validate.Options{Dir: t.dir})
	if err != nil {
		return validate.Attestation{}, err
	}
	meta := v.meta
	meta.SessionID = t.session()
	att, err := build(results, meta)
	if err != nil {
		return att, err
	}
	v.runs = append(v.runs, att)
	for _, s := range results {
		kind := ""
		if s.EnvironmentKind != "" {
			kind = " (" + string(s.EnvironmentKind) + ")"
		}
		vr.logf("%s: validation step %s exited %d in %dms%s", t.prefix, s.Name, s.ExitCode, s.DurationMs, kind)
	}
	if err := writeAttestation(sessionunit.Dir(t.dir), att); err != nil {
		vr.logf("%s: validation: %v", t.prefix, err)
	}
	return att, nil
}

// retryTurn is the builder's one more turn after a failing step (ADR
// 012's hand-back words: fix it and commit, or commit what works and
// hand back), on the budget left: one message (RetryTurnPrompt) naming
// the failed step, the head it ran on and the item's acceptance. The
// build session is resumed with it — a host Claude session, a sandboxed
// one in its own pod (whose HOME still holds the transcript, else it is
// restored from the resume store), a Codex one by `codex exec resume` —
// and when it cannot be (no session id, an adapter without resume, the
// agent refusing the id) a fresh session gets the brief and the same
// message, saying so in node.log and on the item (goal
// a-failed-check-returns-to-the-session-that-built-it). The turn is
// noted for the heartbeat (RetryTurns) while it runs. An adopted
// session, whose job this process never had, gets none (ran false). The
// turn does not outlive the node: it is the node's, inside the build's
// tail.
func (r *Runner) retryTurn(ctx context.Context, b *buildRun, failed validate.StepResult, head string) (res harness.Result, err error, ran bool) {
	if b.job == nil {
		r.logf("item %s: an adopted session gets no retry turn", b.label)
		return harness.Result{}, nil, false
	}
	turn := *b.job
	turn.HandBackAt, turn.HasCommit, turn.Outlive = 0, nil, false
	if turn.MaxBudgetUSD > 0 {
		turn.MaxBudgetUSD = max(turn.MaxBudgetUSD-b.cost.total(), 0.01)
	}
	if turn.Timeout > 0 {
		turn.Timeout = max(turn.Timeout-time.Since(b.started), 5*time.Minute)
	}
	acceptance := b.item.Acceptance
	if acceptance == "" && b.res != nil {
		acceptance = b.res.Item.Acceptance
	}
	msg := harness.RetryTurnPrompt(harness.RetryTurn{Step: failed.Name, Command: failed.Command, ExitCode: failed.ExitCode, Output: failed.Output,
		Branch: b.spec.Branch, HeadSHA: head, Acceptance: acceptance})
	freshWhy := ""
	fresh := func(why string) {
		freshWhy = why
		note := "session not resumed (" + why + "); fresh session with the failure"
		r.logf("item %s: %s", b.label, note)
		b.report(ctx, note)
		turn.Resume, turn.Prompt = "", b.job.Prompt+"\n\n# Your last session's validation failed\n\n"+msg
	}
	sid := b.session()
	if why := r.unresumable(b, sid); why != "" {
		fresh(why)
	} else {
		turn.Resume, turn.Prompt = sid, msg
	}
	// the turn rides the heartbeat's `resumed` while it runs (ADR-030)
	done := r.noteRetryTurn(b.item.ID, turn.Resume)
	defer func() { done() }()
	b.cost.carry()
	res, err = b.recordedRun(turn, freshWhy, func() (harness.Result, error) { return r.runTurn(ctx, b, turn) })
	if turn.Resume != "" && ctx.Err() == nil && res.SessionID == "" && (err != nil || !res.Success) {
		// the agent never took the session up: the id is unknown to it
		why := "the agent did not resume session " + sid
		if err != nil {
			why += ": " + b.acct.RedactErr(err).Error()
		} else if t := strings.TrimSpace(res.Text); t != "" {
			why += ": " + strings.SplitN(t, "\n", 2)[0]
		}
		fresh(why)
		done()
		done = r.noteRetryTurn(b.item.ID, "")
		b.cost.carry()
		res, err = b.recordedRun(turn, freshWhy, func() (harness.Result, error) { return r.runTurn(ctx, b, turn) })
	}
	return res, err, true
}

// runTurn runs a retry turn's job on the build's harness, its cost and
// session id observed as the build's.
func (r *Runner) runTurn(ctx context.Context, b *buildRun, turn harness.Job) (harness.Result, error) {
	res, err := b.run(ctx, func(sctx context.Context, onProgress func(harness.Progress)) (harness.Result, error) {
		return r.Harness.Run(sctx, turn, onProgress)
	})
	b.cost.observe(res.CostUSD)
	if res.SessionID != "" {
		b.mu.Lock()
		b.sessionID = res.SessionID
		b.mu.Unlock()
	}
	return res, err
}

// noteRetryTurn notes a retry turn on itemID for the heartbeat
// (RetryTurns, Loop.NoteRetryTurn): resuming sessionID, else fresh. Its
// done func is never nil.
func (r *Runner) noteRetryTurn(itemID, sessionID string) func() {
	if r.RetryTurns == nil {
		return func() {}
	}
	if done := r.RetryTurns(itemID, string(harness.KindBuild), sessionID, sessionID == ""); done != nil {
		return done
	}
	return func() {}
}

// unresumable is why the build session sid cannot be resumed for the
// retry turn, "" when it can: no session id, or an adapter with no
// resume. A sandboxed session resumes in its own pod and a host one in
// its own CLAUDE_CONFIG_DIR or CODEX_HOME; when the transcript is gone
// from there the resume store's copy is put back (RestoreTranscript) —
// one that cannot be is left to the agent, whose refusal of the id is
// the fresh fallback.
func (r *Runner) unresumable(b *buildRun, sid string) string {
	if sid == "" {
		return "no session id"
	}
	adapter := ""
	if r.Harness != nil {
		adapter = r.Harness.Name()
	}
	if adapter != AdapterClaudeCode && adapter != AdapterCodex {
		return "adapter " + adapter + " has no resume"
	}
	r.restoreTranscript(b, adapter, sid)
	return ""
}

// restoreTranscript puts the resume store's kept transcript of sid back
// in the turn's config dir when the session's own is not there.
func (r *Runner) restoreTranscript(b *buildRun, adapter, sid string) {
	if r.Resumes == nil {
		return
	}
	dir := turnConfigDir(b, adapter)
	if dir == "" || hasTranscript(dir, adapter, sid) {
		return
	}
	rec, ok := r.Resumes.Lookup(b.item.ID)
	if !ok || rec.SessionID != sid || rec.Transcript == "" {
		return
	}
	if err := harness.RestoreTranscript(dir, adapter, b.dir, sid, rec.Transcript); err != nil {
		r.logf("item %s: %v", b.label, err)
		return
	}
	r.logf("item %s: restored the transcript of session %s", b.label, sid)
}

// turnConfigDir is where the build session's agent keeps its
// transcripts: the pod's CLAUDE_CONFIG_DIR for a sandboxed session, else
// the job's CLAUDE_CONFIG_DIR or CODEX_HOME; "" when the job names none.
func turnConfigDir(b *buildRun, adapter string) string {
	if adapter == AdapterClaudeCode && b.pod != nil && b.pod.Session != nil {
		return b.pod.Session.ClaudeConfigDir()
	}
	key := "CLAUDE_CONFIG_DIR"
	if adapter == AdapterCodex {
		key = "CODEX_HOME"
	}
	for _, kv := range b.job.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// hasTranscript says dir holds sid's transcript where the adapter's
// resume reads it: projects/<workspace>/<sid>.jsonl for Claude Code,
// sessions/YYYY/MM/DD/rollout-…-<sid>.jsonl for Codex.
func hasTranscript(dir, adapter, sid string) bool {
	pattern := filepath.Join(dir, "projects", "*", sid+".jsonl")
	if adapter == AdapterCodex {
		pattern = filepath.Join(dir, "sessions", "*", "*", "*", "rollout-*"+sid+"*")
	}
	m, _ := filepath.Glob(pattern)
	return len(m) > 0
}

// usage is every run's time per step, summed over the runs (the first
// and the retry's), in the order each step first ran, with its last exit
// code: the usage record's validation (UsageReport.Validation).
func (v *buildValidation) usage() []gateway.ValidationMinutes {
	if v == nil {
		return nil
	}
	var out []gateway.ValidationMinutes
	at := map[string]int{}
	for _, att := range v.runs {
		for _, s := range att.Steps {
			i, ok := at[s.Name]
			if !ok {
				i = len(out)
				at[s.Name] = i
				out = append(out, gateway.ValidationMinutes{Step: s.Name})
			}
			out[i].Ms += s.DurationMs
			out[i].ExitCode = s.ExitCode
		}
	}
	return out
}

// attestPushed is every session's end after its last push (goal
// the-node-attests-the-head-it-pushed): the attestation it uploads names
// exactly sha — the head origin has — and that commit's tree. When the
// last run validated another commit (the push landed a different sha, or
// nothing ran yet, as after a rebase) sha is checked out and validated
// again first, saying why, so the platform is never handed a commit that
// is no longer the head. It returns once the upload has completed —
// recorded, refused, unsupported or failed — so the caller reports the
// session finished only after it; an error is a run that produced no
// attestation for sha, never the item's failure.
//
// A refusal naming a newer head — `<sha> is not the head of <branch>
// (GitHub has <X>)`, another push landed on the branch after this one —
// is answered once (answerNewerHead): X fetched, validated and attested.
//
// Runner.attestPushed is the build's attestation of the head it pushed: a
// run that produced none for sha is logged and a note, never the item's
// failure.
func (r *Runner) attestPushed(ctx context.Context, b *buildRun, v *buildValidation, sha string) {
	if _, err := r.validator().attestPushed(ctx, b.target(), v, sha); err != nil {
		msg := "validation: " + b.acct.RedactErr(err).Error() + "; no attestation for " + short(sha)
		r.logf("item %s: %s", b.label, msg)
		b.report(ctx, msg)
	}
}

func (vr validator) attestPushed(ctx context.Context, t validationTarget, v *buildValidation, sha string) (attestOutcome, error) {
	var att validate.Attestation
	if n := len(v.runs); n > 0 {
		att = v.runs[n-1]
	}
	if att.Commit != sha {
		if att.Commit != "" {
			vr.logf("%s: validation ran on %s, the push is %s; validating %s", t.prefix, short(att.Commit), short(sha), short(sha))
		}
		if err := checkoutHead(ctx, t.dir, sha); err != nil {
			return attestOutcome{}, err
		}
		var err error
		if att, err = vr.run(ctx, t, v); err != nil {
			return attestOutcome{}, err
		}
		if att.Commit != sha {
			return attestOutcome{}, fmt.Errorf("validated %s, not the pushed %s", short(att.Commit), short(sha))
		}
	}
	out := vr.upload(ctx, t, att)
	out.Pushed = att
	if branch, newer, ok := newerHead(out.Refused, sha); ok {
		vr.answerNewerHead(ctx, t, v, branch, newer)
	}
	return out, nil
}

// notTheHead is the gateway's refusal of a commit that is no longer its
// branch's head (runsheet gateway/attestation.go, the holder rule):
// `<sha> is not the head of <branch> (GitHub has <X>)`.
var notTheHead = regexp.MustCompile(`([0-9a-f]{7,64}) is not the head of (\S+) \(GitHub has ([0-9a-f]{7,64})\)`)

// newerHead reads a refusal naming another head than sha: the branch and
// the head GitHub has, ok false for any other refusal or one naming sha
// itself.
func newerHead(refused, sha string) (branch, head string, ok bool) {
	m := notTheHead.FindStringSubmatch(refused)
	if m == nil || m[3] == sha {
		return "", "", false
	}
	return m[2], m[3], true
}

// answerNewerHead answers a refusal naming a newer head, once: head is
// fetched from origin's branch under the per-repo fetch lock, checked out
// detached, validated exactly as the pushed head was and its attestation
// uploaded. A head that cannot be fetched, a validation that fails or
// cannot run on it, or a second refusal of any kind is logged `attestation
// refused <reason>` and the session finishes as it would have: never the
// item's failure, never asked again. The checkout goes back where it was.
func (vr validator) answerNewerHead(ctx context.Context, t validationTarget, v *buildValidation, branch, head string) {
	vr.logf("%s: the platform names a newer head of %s, %s; validating it once", t.prefix, branch, short(head))
	refused := func(why string) {
		msg := fmt.Sprintf("newer head %s: %s", short(head), why)
		vr.logf("%s: attestation refused %s", t.prefix, msg)
		vr.Notices.count(true)
		t.report(ctx, "validation attestation refused: "+msg)
	}
	back, err := gitPosition(ctx, t.dir)
	if err != nil {
		refused(err.Error())
		return
	}
	defer func() {
		if err := checkoutHead(ctx, t.dir, back); err != nil {
			vr.logf("%s: validation: back to %s: %v", t.prefix, back, err)
		}
	}()
	if err := vr.fetchHead(ctx, t, branch, head); err != nil {
		refused("fetch: " + t.redact(err).Error())
		return
	}
	if err := checkoutHead(ctx, t.dir, head); err != nil {
		refused(err.Error())
		return
	}
	att, err := vr.run(ctx, t, v)
	if err != nil {
		refused("validation: " + t.redact(err).Error())
		return
	}
	if att.Commit != head {
		refused(fmt.Sprintf("validated %s, not %s", short(att.Commit), short(head)))
		return
	}
	if failed, ok := failedStep(att); ok {
		refused((&validationFailure{Step: failed}).Error())
		return
	}
	vr.upload(ctx, t, att)
}

// headFetcher fetches a branch's head into a checkout (*Git); the
// validator's Workspacer when it is one, else a plain Git.
type headFetcher interface {
	FetchHead(ctx context.Context, dir, branch, head string) error
}

func (vr validator) headFetcher() headFetcher {
	if f, ok := vr.WS.(headFetcher); ok {
		return f
	}
	return &Git{}
}

// changedPaths is the diff against t's base, the base fetched on the
// item's token, and the branch it was taken from. A base origin no
// longer has (*BaseGoneError: a dependency's branch GitHub deleted when
// it merged mid-build) is not the item's failure: the paths are taken
// against origin/<default> instead, which then holds the dependency. A
// squash-merged dependency's commits are not HEAD's ancestors, so they
// count as changed too — a superset of the suites, which is safe: the
// attestation names HEAD, what the platform verifies, whatever selected
// its steps.
func (vr validator) changedPaths(ctx context.Context, t validationTarget) ([]string, string, error) {
	if t.diffFrom != "" {
		paths, err := diffNames(ctx, t.dir, t.diffFrom)
		return paths, t.base, err
	}
	fctx, err := vr.fetchContext(ctx, t)
	if err != nil {
		return nil, t.base, err
	}
	paths, err := vr.WS.ChangedPaths(fctx, t.dir, t.base)
	var gone *BaseGoneError
	if err == nil || !errors.As(err, &gone) || t.defaultBranch == "" || t.base == t.defaultBranch {
		return paths, t.base, err
	}
	paths, err = vr.WS.ChangedPaths(fctx, t.dir, t.defaultBranch)
	return paths, t.defaultBranch, err
}

// diffNames are the files HEAD changed since from, in dir.
func diffNames(ctx context.Context, dir, from string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", from, "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", from, err)
	}
	var paths []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
}

// fetchHead fetches origin's branch and head into t's checkout on the
// item's token.
func (vr validator) fetchHead(ctx context.Context, t validationTarget, branch, head string) error {
	fctx, err := vr.fetchContext(ctx, t)
	if err != nil {
		return err
	}
	return vr.headFetcher().FetchHead(fctx, t.dir, branch, head)
}

// fetchTokenError is a fetch not made because t's token could not be
// had fresh: `github token for <name>: <why>`.
type fetchTokenError struct {
	name string
	err  error
}

func (e *fetchTokenError) Error() string {
	return fmt.Sprintf("github token for %s: %v", e.name, e.err)
}
func (e *fetchTokenError) Unwrap() error { return e.err }

// fetchContext is ctx for a fetch of t's: onItemToken, except that a
// target naming its token (tokenName, a merge batch candidate) is
// fetched on a fresh one or not at all — a renewal refused before any
// of its fetches, the candidate's or the diff base's, is a
// *fetchTokenError, logged `<prefix>: <err>; not fetched`.
func (vr validator) fetchContext(ctx context.Context, t validationTarget) (context.Context, error) {
	if t.tokenName == "" {
		return vr.onItemToken(ctx, t), nil
	}
	fctx, err := vr.onFreshItemToken(ctx, t)
	if err != nil {
		err = &fetchTokenError{name: t.tokenName, err: err}
		vr.logf("%s: %v; not fetched", t.prefix, err)
		return ctx, err
	}
	return fctx, nil
}

// onItemToken is ctx with the validator's git on the item's token, asked
// of the platform when near expiry (ItemCred.ForFetch): the fetches of
// the diff's base and of a newer head go out on it, never on what the
// checkout's helper holds by then — an evaluation's checkout, a rebase's
// or a token since expired. A token that cannot be renewed is logged and
// the one held used; the fallback is ctx.
func (vr validator) onItemToken(ctx context.Context, t validationTarget) context.Context {
	fctx, err := vr.onFreshItemToken(ctx, t)
	if err != nil {
		vr.logf("%s: validation: %v", t.prefix, err)
		return OnItemToken(ctx, vr.WS, t.cred.Token())
	}
	return fctx
}

// onFreshItemToken is onItemToken that answers a token it could not
// renew as the error, ctx unchanged: a merge batch candidate is fetched
// on a fresh token or not at all.
func (vr validator) onFreshItemToken(ctx context.Context, t validationTarget) (context.Context, error) {
	if t.cred == nil && t.repoTok != nil {
		return OnItemToken(ctx, vr.WS, t.repoTok), nil
	}
	tok, err := t.cred.ForFetch(ctx)
	if err != nil {
		return ctx, err
	}
	return OnItemToken(ctx, vr.WS, tok), nil
}

// gitPosition is where the checkout is: its branch's name, else (a
// detached HEAD) its commit.
func gitPosition(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	return gitHead(ctx, dir)
}

// attestOutcome is what an upload came to: recorded with the receipt's
// status, refused with the gateway's words, or neither (unsupported or
// failed, Err set).
type attestOutcome struct {
	// Pushed is the run attestPushed uploaded: the pushed head's.
	Pushed   validate.Attestation
	Recorded bool
	Status   string
	Refused  string
	Err      error
}

// uploadAttestation hands the platform the run's attestation to sign,
// on the node's own token — the gateway takes a Latchkey machine token
// or an rs_* console token and refuses a GitHub one, and it is the node
// holding the item it checks (runsheet gateway/attestation.go,
// attestingNode); the item's ghs_* token is for git and GitHub alone. A
// platform whose Mutation has no recordValidationAttestation is
// IsUnsupported and said once per run; that is checked first, because
// such an error is also a refusal (gateway.IsUnsupported's doc). A
// platform that has the mutation and refuses the upload (IsRefusal) is
// a note on the item quoting its words verbatim. A spent retry (the
// mutation existed but every attempt failed transiently) or any other
// failure is a note on the item too, never the item's own failure. In
// every case — uploaded, refused, unsupported or failed — a copy stays
// at <workdir>.attestation.json, which the session directory's removal
// leaves. The log says `attestation recorded <sha> <suites> <status>`
// or `attestation refused <reason>`.
func (r *Runner) uploadAttestation(ctx context.Context, b *buildRun, att validate.Attestation) attestOutcome {
	return r.validator().upload(ctx, b.target(), att)
}

func (vr validator) upload(ctx context.Context, t validationTarget, att validate.Attestation) attestOutcome {
	status, err := vr.record(ctx, t, att)
	var out attestOutcome
	switch {
	case err == nil:
		if status == "" {
			status = "no status"
		}
		out = attestOutcome{Recorded: true, Status: status}
		vr.logf("%s: attestation recorded %s %s %s", t.prefix, short(att.Commit), attestedSuites(att), status)
		vr.Notices.count(false)
	case gateway.IsUnsupported(err):
		out.Err = err
		once(vr.Notices.attestation(), func() {
			vr.logf("validation: the platform takes no attestations yet; each is kept beside its checkout as %s (said once, for every item this run)", AttestationFile)
		})
	case gateway.IsRefusal(err):
		out = attestOutcome{Refused: err.Error(), Err: err}
		vr.logf("%s: attestation refused %s", t.prefix, err.Error())
		vr.Notices.count(true)
		t.report(ctx, "validation attestation refused by the platform: "+err.Error())
	default:
		out.Err = err
		note := "validation attestation: upload failed: " + t.redact(err).Error()
		vr.logf("%s: %s", t.prefix, note)
		t.report(ctx, note)
	}
	if err := writeAttestationAt(t.dir+".attestation.json", att); err != nil {
		vr.logf("%s: validation: %v", t.prefix, err)
	}
	return out
}

// record sends att to the platform — an item's by
// RecordValidationAttestation, a batch candidate's (t.batchID) by
// RecordBatchCandidateAttestation — and answers the receipt's status.
func (vr validator) record(ctx context.Context, t validationTarget, att validate.Attestation) (string, error) {
	unsupported := &gateway.Error{Message: "the node's gateway takes no attestations", Unsupported: true}
	if t.batchID != "" {
		rec, ok := vr.GW.(batchAttestationRecorder)
		if !ok {
			return "", unsupported
		}
		receipt, err := rec.RecordBatchCandidateAttestation(ctx, t.batchID, gatewayAttestation(att))
		return receipt.Status, err
	}
	rec := vr.attester()
	if rec == nil {
		return "", unsupported
	}
	receipt, err := rec.RecordValidationAttestation(ctx, t.item.ID, gatewayAttestation(att))
	status, _ := receipt["status"].(string)
	return status, err
}

// attestedSuites are the attestation's steps comma-joined, or `no suite
// affected` when it ran none.
func attestedSuites(att validate.Attestation) string {
	names := make([]string, len(att.Steps))
	for i, s := range att.Steps {
		names[i] = s.Name
	}
	if len(names) == 0 {
		return "no suite affected"
	}
	return strings.Join(names, ",")
}

// ValidationNotices are the node's once-per-run validation notices,
// shared by every copy of its Runner, and the attestation outcomes it
// counts for the health line and `mesh node status` (Counts; nil counts
// nothing).
type ValidationNotices struct {
	noAttestation, noValidationUsage sync.Once
	Counts                           *AttestationCounts
}

// count notes one attestation outcome on the shared counts.
func (n *ValidationNotices) count(refused bool) {
	if n != nil {
		n.Counts.Note(refused)
	}
}

// once says through say once per run (every time on a nil receiver).
func once(o *sync.Once, say func()) {
	if o == nil {
		say()
		return
	}
	o.Do(say)
}

func (n *ValidationNotices) attestation() *sync.Once {
	if n == nil {
		return nil
	}
	return &n.noAttestation
}

func (n *ValidationNotices) usage() *sync.Once {
	if n == nil {
		return nil
	}
	return &n.noValidationUsage
}

// attestationRecorder is what uploads an attestation (*gateway.Client).
type attestationRecorder interface {
	RecordValidationAttestation(ctx context.Context, itemID string, att gateway.ValidationAttestation) (map[string]any, error)
}

// batchAttestationRecorder is what uploads a merge batch candidate's
// attestation (*gateway.Client).
type batchAttestationRecorder interface {
	RecordBatchCandidateAttestation(ctx context.Context, batchID string, att gateway.ValidationAttestation) (gateway.BatchAttestationReceipt, error)
}

// attester is the node's own gateway when it takes attestations, else
// nil. Never the item's token: v0.2.208 and before sent the item's
// ghs_* token and every upload came back "the bearer is a GitHub App
// installation token … a node sends its own Latchkey machine token, as
// its heartbeat does".
func (vr validator) attester() attestationRecorder {
	rec, _ := vr.GW.(attestationRecorder)
	return rec
}

// validationUsage is a finishing command's answer with ErrNoValidationUsage
// taken as landed — the platform has no per-step minutes yet, said once
// per run — and anything else as it is. What else the report landed
// without (ErrNoToolUsage, ErrNoContextUsage, ErrNoUsageDetail, joined with it) goes on to
// Carried.
func (r *Runner) validationUsage(err error) error {
	if !errors.Is(err, gateway.ErrNoValidationUsage) {
		return err
	}
	once(r.Notices.usage(), func() {
		r.logf("validation: the platform's usage report takes no per-step validation minutes yet; sent without them (said once, for every item this run)")
	})
	var rest []error
	for _, lost := range []error{gateway.ErrNoToolUsage, gateway.ErrNoContextUsage, gateway.ErrNoUsageDetail} {
		if errors.Is(err, lost) {
			rest = append(rest, lost)
		}
	}
	return errors.Join(rest...)
}

// gatewayAttestation is validate's attestation in the platform's input
// shape (specs/contracts/recordValidationAttestation.json), field for
// field: one command per step, the tests summed over the steps, each
// artifact as "<path>@sha256:<digest>", the validate.yml's SuiteHash
// (the platform's canonical hash) as the suite hash, and a report per
// step that kept one — 256 KiB in all. A command that failed on the
// node carries its environmentKind (docs/adr/adr-030), left out for a
// pass or a code failure. No step run is no command: an
// empty list, never left out. Whether it ran
// sandboxed, the node and the item stay on the local attestation: the
// platform reads the runner from runnerImage and the node from the
// bearer.
func gatewayAttestation(att validate.Attestation) gateway.ValidationAttestation {
	out := gateway.ValidationAttestation{
		Commit: att.Commit, TreeHash: att.Tree, RunnerImage: att.RunnerImage,
		SuiteHash: att.Validation.Hash, SessionID: att.SessionID, At: att.At,
		ArtifactDigests: []string{}, Commands: []gateway.ValidationCommand{}, Reports: []gateway.ValidationReport{},
	}
	for _, s := range att.Steps {
		out.Commands = append(out.Commands, gateway.ValidationCommand{
			Suite: s.Name, Command: s.Command, ExitCode: s.ExitCode, DurationMs: s.DurationMs,
			EnvironmentKind: string(s.EnvironmentKind),
		})
		out.TestsRun += s.TestsRun
		out.TestsPassed += s.TestsPassed
		out.TestsFailed += s.TestsFailed
		for _, a := range s.Artifacts {
			out.ArtifactDigests = append(out.ArtifactDigests, a.Path+"@sha256:"+a.SHA256)
		}
		if s.Report != "" {
			out.Reports = append(out.Reports, gateway.ValidationReport{Suite: s.Name, Content: s.Report})
		}
	}
	return out
}

// failedStep is the attestation's first failing step.
func failedStep(att validate.Attestation) (validate.StepResult, bool) {
	name, ok := att.Failed()
	if !ok {
		return validate.StepResult{}, false
	}
	for _, s := range att.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return validate.StepResult{Name: name}, true
}

func writeAttestation(sessionDir string, att validate.Attestation) error {
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	return writeAttestationAt(filepath.Join(sessionDir, AttestationFile), att)
}

func writeAttestationAt(path string, att validate.Attestation) error {
	data, err := json.MarshalIndent(att, "", "  ")
	if err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	return nil
}

// gitHead is the checkout's HEAD commit.
func gitHead(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// hostValidationEnv is an unsandboxed validation's environment: the
// session's (the node's, less what the job unsets, plus the job's own)
// with every variable that carries one of its secrets dropped, and
// TMPDIR and GOTMPDIR the run's scratch and GOCACHE the node's build
// cache, each exactly once (scratchEnv).
func hostValidationEnv(job *harness.Job, scratch, cache string) []string {
	env := UserEnv()
	if job == nil {
		return scratchEnv(env, scratch, cache)
	}
	unset := map[string]bool{}
	for _, n := range job.Unset {
		unset[n] = true
	}
	var out []string
	for _, kv := range append(env, job.Env...) {
		name, val, _ := strings.Cut(kv, "=")
		if unset[name] || carriesSecret(val, job.Secrets) {
			continue
		}
		out = append(out, kv)
	}
	return scratchEnv(out, scratch, cache)
}

func carriesSecret(val string, secrets []string) bool {
	for _, s := range secrets {
		if s != "" && strings.Contains(val, s) {
			return true
		}
	}
	return false
}

// sandboxRunner runs a validation step in the session's pod (a fresh
// one of the session's, the agent's having gone with it): the workspace
// mounted and the job's allow-listed env (sandbox.Session.Env). A limit
// kill is the step's failure — 124 for the runtime's timeout, as
// HostRunner's, else 137.
type sandboxRunner struct {
	sess *sandbox.Session
	dir  string
	env  []string
	// cache is the run's scratch and the node's shared cache, mounted in
	// every step's pod and named as its TMPDIR, GOTMPDIR and GOCACHE.
	cache *sandbox.ValidationCache
}

func (s sandboxRunner) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	err := s.sess.Run(ctx, sandbox.RunSpec{Workspace: s.dir, Env: s.env, Cache: s.cache, Command: []string{"sh", "-c", cmd}, Timeout: timeout, Stdout: stdout, Stderr: stderr})
	var ee *sandbox.ExitError
	var le *sandbox.LimitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.Code, nil
	case errors.As(err, &le):
		fmt.Fprintf(stderr, "\n%v\n", le)
		if le.Limit == "timeout" {
			return 124, nil
		}
		return 137, nil
	default:
		return -1, err
	}
}
