package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// ValidateStepName is the validate step's name, and the key its
// attestation is kept under in State.Results.
const ValidateStepName = "validate"

// JobContainer is the job's container as the harness step leaves it on
// State.Pod: the pod its agent ran in and what that agent's container
// saw — image, workdir, mounts, env and limits. The validate step execs
// each suite into the same pod with the same view, so a suite sees what
// the agent saw and nothing runs on the host or in a second pod.
type JobContainer struct {
	Runtime sandbox.Runtime
	Pod     sandbox.Pod
	Image   string
	// Workdir is the workspace's path inside the container; empty is
	// State.WorkspaceDir (the workspace mounted at its own path).
	Workdir string
	Mounts  []sandbox.Mount
	Env     []string
	Limits  sandbox.Limits

	mu    sync.Mutex
	execs int
}

// Validate is the validate step: after the harness step, the repo's
// .runsheet/validate.yml suites that the change since State.BaseRef
// affects (validate.Select), plus the always ones, run by
// validate.Run — only the executor is the job container's
// (JobContainer.Exec). The attestation, sandboxed and naming the job's
// image, is kept in State.Results[ValidateStepName]; a failing suite
// fails the step with its output. A repo with no validate.yml passes
// with no attestation.
type Validate struct {
	// Meta names who ran the validation (node, session, item); the
	// runner's image and Sandboxed are the job container's.
	Meta validate.Meta
	// ChangedPaths is the change the suites are selected by; nil reads
	// `git diff --name-only <base>...HEAD` in the workspace.
	ChangedPaths func(ctx context.Context, dir, base string) ([]string, error)
	// Head is the commit the attestation names; nil reads `git
	// rev-parse HEAD` in the workspace.
	Head func(ctx context.Context, dir string) (string, error)
}

// Step is the validate step for a Job.
func (v Validate) Step() Step {
	return Step{Name: ValidateStepName, Run: v.Run}
}

// Run implements the validate step.
func (v Validate) Run(ctx context.Context, state *State) error {
	jc, ok := state.Pod.(*JobContainer)
	if !ok || jc == nil {
		return fmt.Errorf("validate: no job container on the session (state's pod is %T)", state.Pod)
	}
	dir := state.WorkspaceDir
	cfg, err := validate.Load(dir)
	if errors.Is(err, validate.ErrNoValidation) {
		return nil
	}
	if err != nil {
		return err
	}

	changedPaths := v.ChangedPaths
	if changedPaths == nil {
		changedPaths = gitChangedPaths
	}
	changed, err := changedPaths(ctx, dir, state.BaseRef)
	if err != nil {
		return fmt.Errorf("validate: changed paths: %w", err)
	}
	head := v.Head
	if head == nil {
		head = gitHead
	}
	sha, err := head(ctx, dir)
	if err != nil {
		return fmt.Errorf("validate: head: %w", err)
	}

	results, runErr := validate.Run(ctx, validate.Select(cfg, changed), podRunner{jc: jc, dir: dir}, validate.Options{Dir: dir})

	meta := v.Meta
	meta.RunnerImage = jc.Image
	meta.Sandboxed = true
	att, err := validate.Build(dir, sha, results, meta)
	if err != nil {
		return errors.Join(runErr, fmt.Errorf("validate: attestation: %w", err))
	}
	if state.Results == nil {
		state.Results = map[string]any{}
	}
	state.Results[ValidateStepName] = att

	if runErr != nil {
		return fmt.Errorf("validate: %w", runErr)
	}
	for _, r := range results {
		if r.ExitCode != 0 {
			return &SuiteError{Result: r}
		}
	}
	return nil
}

// SuiteError is a suite that failed in the job container: its result,
// the output included.
type SuiteError struct {
	Result validate.StepResult
}

func (e *SuiteError) Error() string {
	reason := fmt.Sprintf("validation failed: suite %s exited %d", e.Result.Name, e.Result.ExitCode)
	if e.Result.EnvironmentKind != "" {
		reason = validate.WithKind(e.Result.EnvironmentKind, reason)
	}
	if out := strings.TrimSpace(e.Result.Output); out != "" {
		reason += ":\n" + out
	}
	return reason
}

// Exec runs cmd with `sh -c` in a container of the job's pod — the
// agent's image, mounts, env and limits, a container of its own named
// sandbox.ExecName — and returns its exit. A timeout is exit 124, as
// validate.HostRunner's; a kill for memory is 137.
func (jc *JobContainer) Exec(ctx context.Context, workdir, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	if timeout <= 0 {
		timeout = validate.DefaultTimeout
	}
	if jc.Workdir != "" {
		workdir = jc.Workdir
	}
	jc.mu.Lock()
	jc.execs++
	n := jc.execs
	jc.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	exit, err := jc.Runtime.RunAgent(runCtx, jc.Pod, sandbox.AgentSpec{
		Name:           sandbox.ExecName(jc.Pod, n),
		Image:          jc.Image,
		Workdir:        workdir,
		Mounts:         jc.Mounts,
		Env:            jc.Env,
		Command:        []string{"sh", "-c", cmd},
		Limits:         jc.Limits,
		TimeoutSeconds: int((timeout + sandbox.TimeoutGrace) / time.Second),
		Stdout:         stdout,
		Stderr:         stderr,
	})
	switch {
	case ctx.Err() != nil:
		return -1, ctx.Err()
	case runCtx.Err() != nil:
		fmt.Fprintf(stderr, "\nsandbox: suite stopped at its timeout of %s\n", timeout)
		return 124, nil
	case err != nil:
		return -1, fmt.Errorf("sandbox: exec in pod %s: %w", jc.Pod.Name, err)
	case exit.OOMKilled:
		fmt.Fprintf(stderr, "\nsandbox: suite killed for memory (limit %s)\n", jc.Limits.Memory)
		return 137, nil
	}
	return exit.Code, nil
}

// podRunner is validate.Runner on the job container.
type podRunner struct {
	jc  *JobContainer
	dir string
}

func (r podRunner) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	return r.jc.Exec(ctx, r.dir, cmd, timeout, stdout, stderr)
}

func gitChangedPaths(ctx context.Context, dir, base string) ([]string, error) {
	if base == "" {
		return nil, errors.New("no base ref on the session")
	}
	out, err := runGit(ctx, dir, "diff", "--name-only", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

func gitHead(ctx context.Context, dir string) (string, error) {
	return runGit(ctx, dir, "rev-parse", "HEAD")
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
