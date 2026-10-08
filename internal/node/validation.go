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
}

// validationFailure is a step that failed on the commit the build
// would finish with: the item's failure, naming the step first.
type validationFailure struct {
	Step validate.StepResult
}

func (e *validationFailure) Error() string {
	return fmt.Sprintf("validation failed: step %s exited %d", e.Step.Name, e.Step.ExitCode)
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
}

func (r *Runner) validator() validator {
	return validator{GW: r.GW, WS: r.WS, Scopes: r.Scopes, Sandbox: r.Sandbox, Inspect: r.Inspect, Notices: r.Notices, logf: r.logf}
}

// target is the build as its validation needs it.
func (b *buildRun) target() validationTarget {
	return validationTarget{item: b.item, dir: b.dir, base: b.spec.BaseRef, prefix: "item " + b.label, label: b.label,
		job: b.job, pod: b.podSession(), session: b.session, redact: b.acct.RedactErr, report: b.report}
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
	if vr.Sandbox == nil {
		host, _ := os.Hostname()
		v.runner = validate.HostRunner{Dir: t.dir, Env: hostValidationEnv(t.job)}
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
		s, err := vr.Sandbox.Open(t.label, "validate", git)
		if err != nil {
			return nil, err
		}
		sess, v.close = s, func() { _ = s.Close() }
	}
	var env []string
	if t.job != nil {
		env = t.job.Env
	}
	v.runner = sandboxRunner{sess: sess, dir: t.dir, env: env}
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
	r.logf("item %s: %v; one more turn", b.label, vf)
	b.report(ctx, vf.Error()+"; the builder gets one more turn")
	res, runErr, ran := r.retryTurn(ctx, b, failed)
	if !ran {
		return att, vf
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
	cfg, err := validate.Load(t.dir)
	if err != nil {
		return validate.Attestation{}, err
	}
	changed, err := vr.WS.ChangedPaths(ctx, t.dir, t.base)
	if err != nil {
		return validate.Attestation{}, fmt.Errorf("changed paths: %w", err)
	}
	head, err := gitHead(ctx, t.dir)
	if err != nil {
		return validate.Attestation{}, err
	}
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
	att, err := validate.Build(t.dir, head, results, meta)
	if err != nil {
		return att, err
	}
	v.runs = append(v.runs, att)
	for _, s := range results {
		vr.logf("%s: validation step %s exited %d in %dms", t.prefix, s.Name, s.ExitCode, s.DurationMs)
	}
	if err := writeAttestation(sessionunit.Dir(t.dir), att); err != nil {
		vr.logf("%s: validation: %v", t.prefix, err)
	}
	return att, nil
}

// retryTurn is the builder's one more turn after a failing step (ADR
// 012's hand-back words: fix it and commit, or commit what works and
// hand back): the session resumed with ValidationRetryPrompt on the
// budget left when it is a host Claude one whose id is known, else a
// fresh session told the failure after its brief. An adopted session,
// whose job this process never had, gets none (ran false). The turn does
// not outlive the node: it is the node's, inside the build's tail.
func (r *Runner) retryTurn(ctx context.Context, b *buildRun, failed validate.StepResult) (res harness.Result, err error, ran bool) {
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
	prompt := harness.ValidationRetryPrompt(failed.Name, failed.Command, failed.ExitCode, failed.Output)
	if sid := b.session(); sid != "" && r.Sandbox == nil && Adapter(r.Harness) == AdapterClaudeCode {
		turn.Resume, turn.Prompt = sid, prompt
	} else {
		turn.Resume, turn.Prompt = "", b.job.Prompt+"\n\n# Your last session's validation failed\n\n"+prompt
	}
	b.cost.carry()
	res, err = b.run(ctx, func(onProgress func(harness.Progress)) (harness.Result, error) {
		return r.Harness.Run(ctx, turn, onProgress)
	})
	b.cost.observe(res.CostUSD)
	if res.SessionID != "" {
		b.mu.Lock()
		b.sessionID = res.SessionID
		b.mu.Unlock()
	}
	return res, err, true
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
// attestPushed is the build's attestation of the head it pushed: a run
// that produced none for sha is logged and a note, never the item's
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
	return vr.upload(ctx, t, att), nil
}

// attestOutcome is what an upload came to: recorded with the receipt's
// status, refused with the gateway's words, or neither (unsupported or
// failed, Err set).
type attestOutcome struct {
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
	rec := vr.attester()
	var err error
	var receipt map[string]any
	if rec == nil {
		err = &gateway.Error{Message: "the node's gateway takes no attestations", Unsupported: true}
	} else {
		receipt, err = rec.RecordValidationAttestation(ctx, t.item.ID, gatewayAttestation(att))
	}
	var out attestOutcome
	switch {
	case err == nil:
		status, _ := receipt["status"].(string)
		if status == "" {
			status = "no status"
		}
		out = attestOutcome{Recorded: true, Status: status}
		vr.logf("%s: attestation recorded %s %s %s", t.prefix, short(att.Commit), attestedSuites(att), status)
	case gateway.IsUnsupported(err):
		out.Err = err
		once(vr.Notices.attestation(), func() {
			vr.logf("validation: the platform takes no attestations yet; each is kept beside its checkout as %s (said once, for every item this run)", AttestationFile)
		})
	case gateway.IsRefusal(err):
		out = attestOutcome{Refused: err.Error(), Err: err}
		vr.logf("%s: attestation refused %s", t.prefix, err.Error())
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
// shared by every copy of its Runner.
type ValidationNotices struct {
	noAttestation, noValidationUsage sync.Once
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
// step that kept one — 256 KiB in all. No step run is no command: an
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
// with every variable that carries one of its secrets dropped.
func hostValidationEnv(job *harness.Job) []string {
	env := UserEnv()
	if job == nil {
		return env
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
	return out
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
}

func (s sandboxRunner) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	err := s.sess.Run(ctx, sandbox.RunSpec{Workspace: s.dir, Env: s.env, Command: []string{"sh", "-c", cmd}, Timeout: timeout, Stdout: stdout, Stderr: stderr})
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
