package validate

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// retryFake is a fake LaneRunner whose main command streams one unit's
// go-test-json lines directly into the given stdout, checking ctx
// between each so a stop mid-stream is honoured (the lines after the
// stop point are never written, as a real process killed there never
// writes them); a command naming "-run" (the by-name retry) streams
// its own, separate lines instead. Every invocation is recorded.
type retryFake struct {
	mu    sync.Mutex
	calls []string

	main  []string
	retry []string
}

func (f *retryFake) Run(ctx context.Context, cmd string, _ time.Duration, stdout, _ io.Writer) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()

	lines := f.main
	if strings.Contains(cmd, "-run") {
		lines = f.retry
	}
	exit := 0
	for _, line := range lines {
		if ctx.Err() != nil {
			return 1, ctx.Err()
		}
		fmt.Fprintln(stdout, line)
		if strings.Contains(line, `"fail"`) {
			exit = 1
		}
	}
	if ctx.Err() != nil {
		return 1, ctx.Err()
	}
	return exit, nil
}

func (f *retryFake) Lane(context.Context, string) (Runner, func(), error) {
	return f, func() {}, nil
}

func (f *retryFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const (
	retryFooFail = `{"Time":"2026-11-11T00:00:00Z","Action":"run","Package":"pkg","Test":"TestFoo"}
{"Time":"2026-11-11T00:00:00Z","Action":"fail","Package":"pkg","Test":"TestFoo","Elapsed":0.01}`
	retryBarPass = `{"Time":"2026-11-11T00:00:00Z","Action":"run","Package":"pkg","Test":"TestBar"}
{"Time":"2026-11-11T00:00:00Z","Action":"pass","Package":"pkg","Test":"TestBar","Elapsed":0.01}`
	retryFooPass = `{"Time":"2026-11-11T00:00:00Z","Action":"run","Package":"pkg","Test":"TestFoo"}
{"Time":"2026-11-11T00:00:00Z","Action":"pass","Package":"pkg","Test":"TestFoo","Elapsed":0.01}`
)

func retryStep(extra ...func(*Step)) Step {
	st := Step{
		Name:    "unit",
		Command: "go test ./pkg/...",
		Timeout: time.Minute,
		Retry:   "go test -run '^{test}$' {package}",
	}
	for _, f := range extra {
		f(&st)
	}
	return st
}

func testNamed(tests []TestResult, name string) *TestResult {
	for i := range tests {
		if tests[i].Name == name {
			return &tests[i]
		}
	}
	return nil
}

// A failing test streamed off a unit with a Retry configured is
// retried by name alone, never the whole unit; when the retry passes,
// the test is marked flaky and the unit runs on, every later test
// still appearing.
func TestShardSupervisorRetriesAFailingTestByNameAndMarksItFlaky(t *testing.T) {
	dir := t.TempDir()
	r := &retryFake{
		main:  strings.Split(retryFooFail+"\n"+retryBarPass, "\n"),
		retry: strings.Split(retryFooPass, "\n"),
	}
	steps := []Step{retryStep()}

	results, err := Run(context.Background(), steps, r, Options{Dir: dir, ContinueOnFailure: true})
	if err != nil {
		t.Fatalf("Run() err = %v, want no runner error", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want one step", results)
	}
	sr := results[0]
	if sr.Stopped {
		t.Fatalf("StepResult.Stopped = true, want false: the retry passed, the test is flaky not confirmed")
	}
	foo := testNamed(sr.Tests, "TestFoo")
	if foo == nil || foo.Action != "fail" || foo.Retry == nil || foo.Retry.Action != "pass" {
		t.Fatalf("TestFoo = %+v, want Action fail with a non-nil, passing Retry", foo)
	}
	bar := testNamed(sr.Tests, "TestBar")
	if bar == nil || bar.Action != "pass" {
		t.Fatalf("TestBar = %+v, want present and passing: the whole unit was not re-run, only the retry", bar)
	}
	if got := r.callCount(); got != 2 {
		t.Fatalf("Run invocations = %d (%v), want 2: the main command and one by-name retry", got, r.calls)
	}
}

// A failing test whose retry fails too is confirmed, and the lane is
// stopped there by default: the test's own Retry still carries the
// retry's failure, StopReason names the test, and nothing the unit
// would have streamed after the failure appears — the command was
// cancelled, not re-run from the start.
func TestShardSupervisorStopsTheLaneOnAConfirmedRealFailure(t *testing.T) {
	dir := t.TempDir()
	r := &retryFake{
		main:  strings.Split(retryFooFail+"\n"+retryBarPass, "\n"),
		retry: strings.Split(retryFooFail, "\n"),
	}
	steps := []Step{retryStep()}

	results, err := Run(context.Background(), steps, r, Options{Dir: dir, ContinueOnFailure: true})
	if err != nil {
		t.Fatalf("Run() err = %v, want no runner error from a deliberate stop", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want one step", results)
	}
	sr := results[0]
	if !sr.Stopped {
		t.Fatalf("StepResult.Stopped = false, want true: the retry confirmed the failure")
	}
	if !strings.Contains(sr.StopReason, "TestFoo") {
		t.Fatalf("StopReason = %q, want it to name TestFoo", sr.StopReason)
	}
	foo := testNamed(sr.Tests, "TestFoo")
	if foo == nil || foo.Action != "fail" || foo.Retry == nil || foo.Retry.Action != "fail" {
		t.Fatalf("TestFoo = %+v, want Action fail with a non-nil, failing Retry", foo)
	}
	if bar := testNamed(sr.Tests, "TestBar"); bar != nil {
		t.Fatalf("Tests = %+v, want TestBar absent: the unit was stopped before it streamed", sr.Tests)
	}
	if got := r.callCount(); got != 2 {
		t.Fatalf("Run invocations = %d (%v), want 2 (main + one by-name retry, never a third)", got, r.calls)
	}
}

// The same confirmed failure, but the step runs to its end: the lane
// is not stopped, every test the unit streams — before and after the
// confirmed failure — appears, and the failed test's Retry still
// carries the retry's own failure.
func TestShardSupervisorRunsToEndWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	r := &retryFake{
		main:  strings.Split(retryFooFail+"\n"+retryBarPass, "\n"),
		retry: strings.Split(retryFooFail, "\n"),
	}
	steps := []Step{retryStep(func(s *Step) { s.RunToEnd = true })}

	results, err := Run(context.Background(), steps, r, Options{Dir: dir, ContinueOnFailure: true})
	if err != nil {
		t.Fatalf("Run() err = %v, want no runner error", err)
	}
	sr := results[0]
	if sr.Stopped {
		t.Fatalf("StepResult.Stopped = true, want false: RunToEnd keeps the unit running past a confirmed failure")
	}
	foo := testNamed(sr.Tests, "TestFoo")
	if foo == nil || foo.Action != "fail" || foo.Retry == nil || foo.Retry.Action != "fail" {
		t.Fatalf("TestFoo = %+v, want Action fail with a non-nil, failing Retry", foo)
	}
	bar := testNamed(sr.Tests, "TestBar")
	if bar == nil || bar.Action != "pass" {
		t.Fatalf("TestBar = %+v, want present: the unit ran to its end", bar)
	}
	if got := r.callCount(); got != 2 {
		t.Fatalf("Run invocations = %d (%v), want 2: the main command and one by-name retry", got, r.calls)
	}
}

// Parse reads a suite's retry and run_to_end keys verbatim, leaving
// both at their zero value for a suite naming neither.
func TestParseAcceptsRetryAndRunToEnd(t *testing.T) {
	data := []byte(`
suites:
  - name: unit
    command: go test ./...
    retry: "go test -run '^{test}$' {package}"
    run_to_end: true
  - name: other
    command: go test ./other/...
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() err = %v", err)
	}
	if len(cfg.Steps) != 2 {
		t.Fatalf("Steps = %+v, want 2", cfg.Steps)
	}
	if got, want := cfg.Steps[0].Retry, "go test -run '^{test}$' {package}"; got != want {
		t.Fatalf("Steps[0].Retry = %q, want %q verbatim", got, want)
	}
	if !cfg.Steps[0].RunToEnd {
		t.Fatalf("Steps[0].RunToEnd = false, want true")
	}
	if cfg.Steps[1].Retry != "" {
		t.Fatalf("Steps[1].Retry = %q, want empty: the suite names no retry", cfg.Steps[1].Retry)
	}
	if cfg.Steps[1].RunToEnd {
		t.Fatalf("Steps[1].RunToEnd = true, want false: the suite names no run_to_end")
	}
}
