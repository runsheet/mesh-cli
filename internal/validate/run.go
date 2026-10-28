package validate

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
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
	// Redact masks what a step's output must not carry — the values of
	// the run's environment — before it is kept on the StepResult (Output
	// and Tail); nil keeps it as it is. Token shapes are masked either
	// way (RedactTokens).
	Redact func(string) string
}

// outputCap is the total bytes of a step's combined stdout+stderr kept
// on its StepResult — the head and the tail, split evenly for a passing
// step, and mostly the tail for a failing one, where the cause is.
const outputCap = 8 * 1024

// TailLines and tailCap bound a failing step's Tail: its last 200
// lines, in at most 32 KiB.
const (
	TailLines = 200
	tailCap   = 32 * 1024
)

// tokenShapes are credentials by their look, masked in any step output
// whatever the run's environment named: GitHub's (ghs_, ghp_, gho_,
// ghu_, ghr_, github_pat_), runsheet's rs_, Anthropic's and OpenAI's
// sk- keys, and a bearer header's value.
var tokenShapes = regexp.MustCompile(`\b(gh[spour]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,}|rs_[A-Za-z0-9_-]{16,}|sk-[A-Za-z0-9_-]{20,})|(?i:bearer\s+)[A-Za-z0-9._~+/=-]{16,}`)

// RedactTokens masks every token-shaped string in s.
func RedactTokens(s string) string {
	return tokenShapes.ReplaceAllString(s, "[redacted]")
}

// lastLines is the last n lines of s, within max bytes.
func lastLines(s string, n, max int) string {
	s = strings.TrimRight(s, "\n")
	i := len(s)
	for k := 0; k < n && i > 0; k++ {
		j := strings.LastIndexByte(s[:i], '\n')
		if j < 0 {
			i = 0
			break
		}
		i = j
	}
	out := s[i:]
	out = strings.TrimPrefix(out, "\n")
	if len(out) > max {
		out = out[len(out)-max:]
		if j := strings.IndexByte(out, '\n'); j >= 0 {
			out = out[j+1:]
		}
	}
	return out
}

// LaneRunner is a Runner that can make another runner beside itself:
// a pod of its own, with its own services, for a group or a shard to run
// in while the others run (Run). release removes it.
type LaneRunner interface {
	Runner
	Lane(ctx context.Context, name string) (r Runner, release func(), err error)
}

// unit is one command Run runs: a step, or one shard of it.
type unit struct {
	step  int
	shard int
	total int
}

// ran is what one unit's command did.
type ran struct {
	stdout, stderr bytes.Buffer
	exitCode       int
	runErr         error
	duration       time.Duration
}

// lane is the units that run one after another in one runner.
type lane struct {
	name  string
	units []unit
}

// lanes is the steps cut into lanes: one per group, in the order the
// groups first appear, and one per shard of each sharded step. The
// groups are the platform's (validatespec.Spec.Groups): with no step
// naming a group every step is in one, "validate"; once one does, a step
// naming none is in the group of its kind. Without a LaneRunner (split
// false) every unit is in one lane, in the file's order, shards one
// after another.
func lanes(steps []Step, split bool) []lane {
	var out []lane
	at := map[string]int{}
	grouped := slices.ContainsFunc(steps, func(st Step) bool { return st.Group != "" })
	for i, st := range steps {
		n := max(st.Shards, 1)
		if !split {
			if len(out) == 0 {
				out = append(out, lane{})
			}
			for k := range n {
				out[0].units = append(out[0].units, unit{step: i, shard: k, total: n})
			}
			continue
		}
		if n > 1 {
			for k := range n {
				out = append(out, lane{name: fmt.Sprintf("%s-%d", st.Name, k), units: []unit{{step: i, shard: k, total: n}}})
			}
			continue
		}
		name := "validate"
		switch {
		case st.Group != "":
			name = st.Group
		case grouped:
			name = cmp.Or(string(st.Kind), "validate")
		}
		j, ok := at[name]
		if !ok {
			j = len(out)
			at[name] = j
			out = append(out, lane{name: name})
		}
		out[j].units = append(out[j].units, unit{step: i, total: 1})
	}
	return out
}

// shardCommand is a shard's command: its index and the shard count
// exported before it.
func shardCommand(cmd string, u unit) string {
	if u.total <= 1 {
		return cmd
	}
	return fmt.Sprintf("export %s=%d %s=%d; %s", ShardIndexEnv, u.shard, ShardTotalEnv, u.total, cmd)
}

// Run runs the steps through runner, recording a StepResult for each in
// the steps' order. Steps of one group run one after another, the first
// one failing (its command erring or exiting non-zero) stopping its
// group unless opts.ContinueOnFailure; when runner is a LaneRunner each
// group, and each shard of a step declaring shards, runs beside the
// others in a lane of its own (LaneRunner.Lane), the first in runner
// itself. Otherwise every step runs in runner in the file's order, a
// sharded step's shards one after another, as before groups and shards.
// A sharded step is one StepResult: it fails when a shard does, its
// duration the longest shard's (the wall), its tests the shards' sum,
// its output each shard's line and then a failing shard's output.
// A failing step carries its EnvironmentKind (EnvironmentKindOf of its
// exit and the runner's error), and a runner's error is returned
// classified (Classify). Each step's report is cut to what is left of
// reportCap after the steps before it.
func Run(ctx context.Context, steps []Step, runner Runner, opts Options) ([]StepResult, error) {
	lr, split := runner.(LaneRunner)
	ls := lanes(steps, split)
	if len(ls) <= 1 {
		split = false
	}
	done := make(map[unit]*ran)
	var mu sync.Mutex
	var laneErr error
	runLane := func(r Runner, l lane) {
		for _, u := range l.units {
			st := steps[u.step]
			out := &ran{}
			start := time.Now()
			// the suite's own cpus and memory, every shard alike
			out.exitCode, out.runErr = r.Run(WithResources(ctx, st.Resources()), shardCommand(st.Command, u), st.Timeout, &out.stdout, &out.stderr)
			out.duration = time.Since(start)
			mu.Lock()
			done[u] = out
			mu.Unlock()
			if out.runErr != nil || (out.exitCode != 0 && !opts.ContinueOnFailure) {
				return
			}
		}
	}
	if split {
		var wg sync.WaitGroup
		for i, l := range ls {
			wg.Add(1)
			go func(i int, l lane) {
				defer wg.Done()
				if i == 0 {
					runLane(runner, l)
					return
				}
				r, release, err := lr.Lane(ctx, l.name)
				if err != nil {
					mu.Lock()
					laneErr = cmp.Or(laneErr, fmt.Errorf("lane %s: %w", l.name, err))
					mu.Unlock()
					return
				}
				defer release()
				runLane(r, l)
			}(i, l)
		}
		wg.Wait()
	} else if len(ls) == 1 {
		// one lane: the first failure stops the whole run, as before
		runLane(runner, ls[0])
	}

	results := make([]StepResult, 0, len(steps))
	room := reportCap
	var runErr error
	for i, st := range steps {
		n := max(st.Shards, 1)
		var shards []ShardResult
		for k := range n {
			out, ok := done[unit{step: i, shard: k, total: n}]
			if !ok {
				continue
			}
			sh := st
			if n > 1 {
				sh = st.shard(k)
			}
			artifacts, err := digestArtifacts(opts.Dir, st.Artifacts)
			if err != nil {
				return results, err
			}
			run, passed, failed, report, err := parseResults(opts.Dir, sh, out.stdout.Bytes())
			if err != nil {
				return results, err
			}
			sr := StepResult{
				Name:        st.Name,
				Command:     st.Command,
				ExitCode:    out.exitCode,
				DurationMs:  out.duration.Milliseconds(),
				TestsRun:    run,
				TestsPassed: passed,
				TestsFailed: failed,
				Report:      report,
				TimedOut:    out.exitCode == 124,
				Artifacts:   artifacts,
			}
			combined := RedactTokens(out.stdout.String() + out.stderr.String())
			if opts.Redact != nil {
				combined = opts.Redact(combined)
			}
			if out.exitCode != 0 || out.runErr != nil {
				sr.Output = failTail(combined)
				sr.Tail = failureTail(combined)
				sr.EnvironmentKind = EnvironmentKindOf(sr, out.runErr)
			} else {
				sr.Output = headTail(combined)
			}
			if out.runErr != nil && runErr == nil {
				runErr = Classify(out.runErr)
			}
			shards = append(shards, ShardResult{Index: k, Total: n, StepResult: sr})
		}
		if len(shards) == 0 {
			continue
		}
		sr := merge(shards, split)
		if len(sr.Report) > room {
			sr.Report = sr.Report[:room]
		}
		room -= len(sr.Report)
		results = append(results, sr)
	}
	if runErr != nil {
		return results, runErr
	}
	if laneErr != nil {
		return results, Classify(laneErr)
	}
	return results, nil
}

// ShardResult is one shard's result, kept on its step's StepResult for
// the node's log (StepResult.Shards); the attestation carries the merged
// step alone.
type ShardResult struct {
	Index int
	Total int
	StepResult
}

// merge is a step's one StepResult from its shards' (one, for a step
// with no shards): the first failing shard's exit, environment kind and
// output after a line per shard; the wall (the longest shard when they
// ran at once, else their sum); the tests and reports summed in shard
// order.
func merge(shards []ShardResult, concurrent bool) StepResult {
	if len(shards) == 1 && shards[0].Total <= 1 {
		return shards[0].StepResult
	}
	out := StepResult{Name: shards[0].Name, Command: shards[0].Command, Artifacts: shards[0].Artifacts, Shards: shards}
	var lines, report strings.Builder
	failed := -1
	for i, s := range shards {
		fmt.Fprintf(&lines, "shard %d/%d: exit %d in %dms, %d tests (%d failed)\n", s.Index+1, s.Total, s.ExitCode, s.DurationMs, s.TestsRun, s.TestsFailed)
		if concurrent {
			out.DurationMs = max(out.DurationMs, s.DurationMs)
		} else {
			out.DurationMs += s.DurationMs
		}
		out.TestsRun += s.TestsRun
		out.TestsPassed += s.TestsPassed
		out.TestsFailed += s.TestsFailed
		out.TimedOut = out.TimedOut || s.TimedOut
		report.WriteString(s.Report)
		if s.ExitCode != 0 && failed < 0 {
			failed = i
		}
	}
	out.Report = report.String()
	if len(shards) < shards[0].Total && failed < 0 {
		// a shard that never ran (its lane did not open) is no pass
		out.ExitCode = -1
	}
	if failed >= 0 {
		f := shards[failed]
		// a failing shard's tail is the step's: what the node logs
		out.ExitCode, out.EnvironmentKind, out.Tail = f.ExitCode, f.EnvironmentKind, f.Tail
		lines.WriteString(fmt.Sprintf("shard %d/%d failed:\n", f.Index+1, f.Total))
		lines.WriteString(f.Output)
		out.Output = failTail(lines.String())
		return out
	}
	out.Output = headTail(lines.String())
	return out
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

// failTail is a failing step's Output: a short head (the command's
// first words, a build's first error) and the rest of outputCap from
// the tail, where a test run's failures and summary are.
func failTail(combined string) string {
	if len(combined) <= outputCap {
		return combined
	}
	head := outputCap / 8
	return combined[:head] + "\n...\n" + combined[len(combined)-(outputCap-head):]
}

func headTail(combined string) string {
	if len(combined) <= outputCap {
		return combined
	}
	half := outputCap / 2
	return combined[:half] + "\n...\n" + combined[len(combined)-half:]
}
