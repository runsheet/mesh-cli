package validate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runner runs one step's command with a timeout, streaming its output.
// The exit code is what the process (or, on a timeout, the runner's own
// convention — HostRunner uses 124, matching GNU timeout) reports; err is
// an infrastructure failure, not the command's own exit status.
type Runner interface {
	Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (exitCode int, err error)
}

// HostRunner runs a step's command with `sh -c` directly in Dir — the
// unsandboxed path, until the sandbox runtime is present.
type HostRunner struct {
	Dir string
	// Env is the steps' whole environment; nil inherits the process's.
	Env []string
}

// Run implements Runner: the command is killed at the timeout, reporting
// exit code 124 (the convention `timeout` itself uses). The command runs
// in its own process group (setProcessGroup) so a step that forks — a
// shell script running a further command — is killed whole: killing only
// the shell would leave its child running and holding stdout/stderr open,
// hanging Wait indefinitely.
func (r HostRunner) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	c := exec.Command("sh", "-c", cmd)
	c.Dir = r.Dir
	c.Env = r.Env
	c.Stdout = stdout
	c.Stderr = stderr
	setProcessGroup(c)

	if err := c.Start(); err != nil {
		return -1, err
	}

	done := make(chan error, 1)
	go func() { done <- c.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-done:
		if err == nil {
			return 0, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return -1, err
	case <-timer.C:
		killGroup(c.Process.Pid)
		<-done
		return 124, nil
	case <-ctx.Done():
		killGroup(c.Process.Pid)
		<-done
		return -1, ctx.Err()
	}
}

// Options configures Run.
type Options struct {
	// Dir is the workspace the steps ran (and run) in — results files
	// and artifact digests are relative to it.
	Dir string
	// ContinueOnFailure runs every selected step regardless of an
	// earlier failure; otherwise the first failing step stops the run.
	ContinueOnFailure bool
}

// outputCap is the total bytes of a step's combined stdout+stderr kept
// on its StepResult — the head and the tail, split evenly.
const outputCap = 8 * 1024

// Run runs each step through runner in order, recording a StepResult for
// each. The first step whose command errors or exits non-zero stops the
// run (its result is still recorded) unless opts.ContinueOnFailure.
// Each step's report is cut to what is left of reportCap after the
// steps before it.
func Run(ctx context.Context, steps []Step, runner Runner, opts Options) ([]StepResult, error) {
	results := make([]StepResult, 0, len(steps))
	room := reportCap
	for _, step := range steps {
		var stdout, stderr bytes.Buffer
		start := time.Now()
		exitCode, runErr := runner.Run(ctx, step.Command, step.Timeout, &stdout, &stderr)
		duration := time.Since(start)

		artifacts, err := digestArtifacts(opts.Dir, step.Artifacts)
		if err != nil {
			return results, err
		}
		run, passed, failed, report, err := parseResults(opts.Dir, step, stdout.Bytes())
		if err != nil {
			return results, err
		}
		if len(report) > room {
			report = report[:room]
		}
		room -= len(report)

		sr := StepResult{
			Name:        step.Name,
			Command:     step.Command,
			ExitCode:    exitCode,
			DurationMs:  duration.Milliseconds(),
			TestsRun:    run,
			TestsPassed: passed,
			TestsFailed: failed,
			Output:      headTail(stdout.String() + stderr.String()),
			Report:      report,
			TimedOut:    exitCode == 124,
			Artifacts:   artifacts,
		}
		results = append(results, sr)

		if runErr != nil {
			return results, runErr
		}
		if exitCode != 0 && !opts.ContinueOnFailure {
			return results, nil
		}
	}
	return results, nil
}

// reportCap is the most of a run's raw reports kept on its StepResults,
// in all: the platform keeps 256 KiB of reports per attestation
// (runsheet ADR 032).
const reportCap = 256 * 1024

// parseResults parses a step's test output per its Results field: empty
// parses nothing, "stdout" reads stdout as go test -json, "go-test-json:
// <path>" and "junit:<path>" read that file (relative to dir). The raw
// report it parsed is returned beside the counts, cut at reportCap. A
// step with no results but reports is the files it names that exist,
// one after another, counted as no tests.
func parseResults(dir string, step Step, stdout []byte) (run, passed, failed int, report string, err error) {
	var raw []byte
	parse := ParseGoTestJSON
	switch {
	case step.Results == "stdout":
		raw = stdout
	case strings.HasPrefix(step.Results, "go-test-json:"):
		if raw, err = os.ReadFile(filepath.Join(dir, strings.TrimPrefix(step.Results, "go-test-json:"))); err != nil {
			return 0, 0, 0, "", err
		}
	case strings.HasPrefix(step.Results, "junit:"):
		if raw, err = os.ReadFile(filepath.Join(dir, strings.TrimPrefix(step.Results, "junit:"))); err != nil {
			return 0, 0, 0, "", err
		}
		parse = ParseJUnit
	default:
		return 0, 0, 0, readReports(dir, step.Reports), nil
	}
	run, passed, failed, err = parse(bytes.NewReader(raw))
	if len(raw) > reportCap {
		raw = raw[:reportCap]
	}
	return run, passed, failed, string(raw), err
}

// readReports is the content of each of reports under dir that exists,
// in order, cut at reportCap.
func readReports(dir string, reports []string) string {
	var b strings.Builder
	for _, r := range reports {
		data, err := os.ReadFile(filepath.Join(dir, r))
		if err != nil {
			continue
		}
		b.Write(data)
		if b.Len() >= reportCap {
			return b.String()[:reportCap]
		}
	}
	return b.String()
}

func headTail(combined string) string {
	if len(combined) <= outputCap {
		return combined
	}
	half := outputCap / 2
	return combined[:half] + "\n...\n" + combined[len(combined)-half:]
}
