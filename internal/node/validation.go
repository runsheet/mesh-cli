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
// commit the item finishes with — the steps the branch's diff touches
// plus every `always` one — in the session's pod when the node has a
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

// startValidation reads the checkout's validate.yml and readies the
// runner its steps go through: nil when the repository declares none
// (said once per item), the finish as before.
func (r *Runner) startValidation(ctx context.Context, b *buildRun) (*buildValidation, error) {
	if _, err := validate.Load(b.dir); errors.Is(err, validate.ErrNoValidation) {
		r.logf("item %s: no validation declared", b.label)
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	sc, _ := r.Scopes.ByOrg(b.item.OrgID)
	v := &buildValidation{meta: validate.Meta{NodeID: sc.NodeID, ItemID: b.item.ID}, close: func() {}}
	if r.Sandbox == nil {
		host, _ := os.Hostname()
		v.runner = validate.HostRunner{Dir: b.dir, Env: hostValidationEnv(b.job)}
		v.meta.RunnerImage, v.meta.Sandboxed = "host:"+host, false
		return v, nil
	}
	sess := b.podSession()
	if sess == nil {
		// an adopted session's pod ended with the node before it
		s, err := r.Sandbox.Open(b.label, "validate")
		if err != nil {
			return nil, err
		}
		sess, v.close = s, func() { _ = s.Close() }
	}
	var env []string
	if b.job != nil {
		env = b.job.Env
	}
	v.runner = sandboxRunner{sess: sess, dir: b.dir, env: env}
	image := r.Sandbox.Config.Image
	run := r.Inspect
	if run == nil {
		run = sandbox.ExecRunner
	}
	v.meta.RunnerImage, v.meta.Sandboxed = image, true
	if digest, err := sandbox.ImageDigest(ctx, run, image); err != nil {
		r.logf("item %s: validation: %v; the attestation names the image by its ref", b.label, err)
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
// builds, keeps and writes that run's attestation.
func (r *Runner) runValidation(ctx context.Context, b *buildRun, v *buildValidation) (validate.Attestation, error) {
	cfg, err := validate.Load(b.dir)
	if err != nil {
		return validate.Attestation{}, err
	}
	changed, err := r.WS.ChangedPaths(ctx, b.dir, b.spec.BaseRef)
	if err != nil {
		return validate.Attestation{}, fmt.Errorf("changed paths: %w", err)
	}
	head, err := gitHead(ctx, b.dir)
	if err != nil {
		return validate.Attestation{}, err
	}
	steps := validate.Select(cfg, changed)
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	r.logf("item %s: validation on %s: %s", b.label, short(head), strings.Join(names, ", "))
	results, err := validate.Run(ctx, steps, v.runner, validate.Options{Dir: b.dir})
	if err != nil {
		return validate.Attestation{}, err
	}
	meta := v.meta
	meta.SessionID = b.session()
	att, err := validate.Build(b.dir, head, results, meta)
	if err != nil {
		return att, err
	}
	v.runs = append(v.runs, att)
	for _, s := range results {
		r.logf("item %s: validation step %s exited %d in %dms", b.label, s.Name, s.ExitCode, s.DurationMs)
	}
	if err := writeAttestation(sessionunit.Dir(b.dir), att); err != nil {
		r.logf("item %s: validation: %v", b.label, err)
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

// uploadAttestation hands the platform the run's attestation to sign,
// on the node's own token — the gateway takes a Latchkey machine token
// or an rs_* console token and refuses a GitHub one, and it is the node
// holding the item it checks (runsheet gateway/attestation.go,
// attestingNode); the item's ghs_* token is for git and GitHub alone. A
// platform without the endpoint is said once per run; any other failure
// is a note on the item, never its failure. Either keeps a copy at <workdir>.attestation.json, which the
// session directory's removal leaves.
func (r *Runner) uploadAttestation(ctx context.Context, b *buildRun, att validate.Attestation) {
	rec := r.attester()
	var err error
	if rec == nil {
		err = &gateway.Error{Message: "the node's gateway takes no attestations", Unsupported: true}
	} else {
		err = rec.RecordValidationAttestation(ctx, b.item.ID, gatewayAttestation(att))
	}
	switch {
	case err == nil:
		r.logf("item %s: validation attestation for %s uploaded", b.label, short(att.Commit))
		return
	case gateway.IsUnsupported(err):
		once(r.Notices.attestation(), func() {
			r.logf("validation: the platform takes no attestations yet; each is kept beside its checkout as %s (said once, for every item this run)", AttestationFile)
		})
	default:
		note := "validation attestation: upload failed: " + b.acct.RedactErr(err).Error()
		r.logf("item %s: %s", b.label, note)
		b.report(ctx, note)
	}
	if err := writeAttestationAt(b.dir+".attestation.json", att); err != nil {
		r.logf("item %s: validation: %v", b.label, err)
	}
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
	RecordValidationAttestation(ctx context.Context, itemID string, att gateway.ValidationAttestation) error
}

// attester is the node's own gateway when it takes attestations, else
// nil. Never the item's token: v0.2.208 and before sent the item's
// ghs_* token and every upload came back "the bearer is a GitHub App
// installation token … a node sends its own Latchkey machine token, as
// its heartbeat does".
func (r *Runner) attester() attestationRecorder {
	rec, _ := r.GW.(attestationRecorder)
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

// gatewayAttestation is validate's attestation in the gateway's shape,
// field for field, with its own per-step minutes; an artifact is
// "<path>@sha256:<digest>".
func gatewayAttestation(att validate.Attestation) gateway.ValidationAttestation {
	out := gateway.ValidationAttestation{
		Commit: att.Commit, Tree: att.Tree, RunnerImage: att.RunnerImage, Sandboxed: att.Sandboxed,
		NodeID: att.NodeID, SessionID: att.SessionID, ItemID: att.ItemID, At: att.At,
		Steps: []gateway.ValidationStepResult{},
	}
	for _, s := range att.Steps {
		row := gateway.ValidationStepResult{
			Step: s.Name, Command: s.Command, SuiteHash: s.SuiteHash, ExitCode: s.ExitCode, Ms: s.DurationMs,
			TestsRun: s.TestsRun, TestsPassed: s.TestsPassed, TestsFailed: s.TestsFailed,
		}
		for _, a := range s.Artifacts {
			row.Artifacts = append(row.Artifacts, a.Path+"@sha256:"+a.SHA256)
		}
		out.Steps = append(out.Steps, row)
		out.Validation = append(out.Validation, gateway.ValidationMinutes{Step: s.Name, Ms: s.DurationMs, ExitCode: s.ExitCode})
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
