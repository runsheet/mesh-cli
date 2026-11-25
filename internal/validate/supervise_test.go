package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// ev is one go test -json line.
func ev(action, test string, output ...string) string {
	return evIn("pkg", action, test, output...)
}

func evIn(pkg, action, test string, output ...string) string {
	line, _ := json.Marshal(struct {
		Action  string `json:"Action"`
		Package string `json:"Package"`
		Test    string `json:"Test,omitempty"`
		Output  string `json:"Output,omitempty"`
	}{action, pkg, test, strings.Join(output, "")})
	return string(line)
}

// passing and failing are one test's events, start to end.
func passing(test string) []string { return []string{ev("run", test), ev("pass", test)} }
func failing(test string) []string {
	return []string{ev("run", test), ev("output", test, "boom\n"), ev("fail", test)}
}

func cat(parts ...[]string) []string { return slices.Concat(parts...) }

var retryNamesIn = regexp.MustCompile(`export MESH_SUITE_TESTS='([^']*)';`)

// call is one command a supervisedRunner ran.
type call struct {
	cmd     string
	retry   bool
	names   []string
	timeout time.Duration
}

// supervisedRunner is a fake LaneRunner: every lane is the runner
// itself. A command writes the lines its script gives it and exits 1
// when any of them is a fail, 0 otherwise, as go test does; each command
// is recorded, a retry told apart by the selection the supervisor
// exported before it.
type supervisedRunner struct {
	mu    sync.Mutex
	calls []call
	// script is the lines a command writes: c.retry and c.names say
	// whether it is a retry and of which tests; nth counts the retries
	// of that unit so far, from 1.
	script func(c call, nth int) []string
	// hold, when set, is called before a command writes anything.
	hold    func(ctx context.Context, c call)
	retries map[string]int
}

func (f *supervisedRunner) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, _ io.Writer) (int, error) {
	c := call{cmd: cmd, timeout: timeout}
	if m := retryNamesIn.FindStringSubmatch(cmd); m != nil {
		c.retry, c.names = true, strings.Fields(m[1])
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	nth := 0
	if c.retry {
		if f.retries == nil {
			f.retries = map[string]int{}
		}
		unit := retryNamesIn.ReplaceAllString(cmd, "")
		f.retries[unit]++
		nth = f.retries[unit]
	}
	f.mu.Unlock()
	if f.hold != nil {
		f.hold(ctx, c)
	}
	exit := 0
	for _, line := range f.script(c, nth) {
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		fmt.Fprintln(stdout, line)
		if strings.Contains(line, `"Action":"fail"`) {
			exit = 1
		}
	}
	return exit, nil
}

func (f *supervisedRunner) Lane(context.Context, string) (Runner, func(), error) {
	return f, func() {}, nil
}

func (f *supervisedRunner) made() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *supervisedRunner) retried() []call {
	var out []call
	for _, c := range f.made() {
		if c.retry {
			out = append(out, c)
		}
	}
	return out
}

func actionsOf(tests []TestResult) map[string]string {
	out := map[string]string{}
	for _, t := range tests {
		out[t.Name] = t.Action
	}
	return out
}

func retriesOf(tests []TestResult) map[string]int {
	out := map[string]int{}
	for _, t := range tests {
		if t.Retries != 0 {
			out[t.Name] = t.Retries
		}
	}
	return out
}

func manyFailing(n int) []string {
	var out []string
	for i := range n {
		out = append(out, failing(fmt.Sprintf("TestBroken%d", i))...)
	}
	return out
}

// The supervisor's retry, case by case: a red unit's failing tests are
// run again by name — the unit's own command under MESH_SUITE_TESTS —
// after the unit has ended. One that passes is flaky and the unit is
// green; one that fails again is confirmed and stops the run, unless
// its suite runs to the end. "after" is a second suite in the same
// lane: whether it ran says whether the run was stopped early.
func TestRunRetriesFailingTestsByName(t *testing.T) {
	cases := []struct {
		name string
		step Step
		// first is the unit's own run; retry is what its nth retry
		// writes, given the names it was asked for
		first []string
		retry func(names []string, nth int) []string

		wantExit    int
		wantActions map[string]string
		wantRetries map[string]int
		wantFlaky   int
		wantFailed  int
		// wantRetried are the names each retry was asked for, in order
		wantRetried [][]string
		wantAfter   bool
		wantOutput  string
	}{
		{
			name:        "a test that passes on retry is flaky and the unit is green",
			step:        Step{Retries: 1},
			first:       cat(passing("TestOK"), failing("TestFlaky"), []string{evIn("pkg", "fail", "")}),
			retry:       func([]string, int) []string { return passing("TestFlaky") },
			wantExit:    0,
			wantActions: map[string]string{"TestOK": "pass", "TestFlaky": "flaky"},
			wantRetries: map[string]int{"TestFlaky": 1},
			wantFlaky:   1,
			wantRetried: [][]string{{"TestFlaky"}},
			wantAfter:   true,
			wantOutput:  "retry 1 ran TestFlaky (exit 0): 1 passed, 0 failed again",
		},
		{
			name:        "a test that fails again is confirmed and stops the run",
			step:        Step{Retries: 1},
			first:       cat(passing("TestOK"), failing("TestBroken")),
			retry:       func([]string, int) []string { return failing("TestBroken") },
			wantExit:    1,
			wantActions: map[string]string{"TestOK": "pass", "TestBroken": "fail"},
			wantRetries: map[string]int{"TestBroken": 1},
			wantFailed:  1,
			wantRetried: [][]string{{"TestBroken"}},
			wantAfter:   false,
			wantOutput:  "0 passed, 1 failed again",
		},
		{
			name:        "run_to_end keeps the run going past a confirmed failure",
			step:        Step{Retries: 1, RunToEnd: true},
			first:       failing("TestBroken"),
			retry:       func([]string, int) []string { return failing("TestBroken") },
			wantExit:    1,
			wantActions: map[string]string{"TestBroken": "fail"},
			wantRetries: map[string]int{"TestBroken": 1},
			wantFailed:  1,
			wantRetried: [][]string{{"TestBroken"}},
			wantAfter:   true,
		},
		{
			name:        "retries 0 runs nothing again and confirms nothing",
			step:        Step{Retries: 0},
			first:       failing("TestBroken"),
			wantExit:    1,
			wantActions: map[string]string{"TestBroken": "fail"},
			wantFailed:  1,
			wantAfter:   true,
		},
		{
			name:  "retries 2 runs a test still failing a second time, and no third",
			step:  Step{Retries: 2},
			first: cat(failing("TestFlaky"), failing("TestBroken")),
			retry: func(names []string, nth int) []string {
				if nth == 1 {
					return cat(failing("TestFlaky"), failing("TestBroken"))
				}
				return cat(passing("TestFlaky"), failing("TestBroken"))
			},
			wantExit:    1,
			wantActions: map[string]string{"TestFlaky": "flaky", "TestBroken": "fail"},
			wantRetries: map[string]int{"TestFlaky": 2, "TestBroken": 2},
			wantFlaky:   1,
			wantFailed:  1,
			wantRetried: [][]string{{"TestFlaky", "TestBroken"}, {"TestFlaky", "TestBroken"}},
			wantAfter:   false,
		},
		{
			name:  "only the tests still failing are run a second time",
			step:  Step{Retries: 2},
			first: cat(failing("TestA"), failing("TestB")),
			retry: func(names []string, nth int) []string {
				if nth == 1 {
					return cat(passing("TestA"), failing("TestB"))
				}
				return passing("TestB")
			},
			wantExit:    0,
			wantActions: map[string]string{"TestA": "flaky", "TestB": "flaky"},
			wantRetries: map[string]int{"TestA": 1, "TestB": 2},
			wantFlaky:   2,
			wantRetried: [][]string{{"TestA", "TestB"}, {"TestB"}},
			wantAfter:   true,
		},
		{
			name:        "more failing tests than a run retries are not retried: the change is broken",
			step:        Step{Retries: 1},
			first:       manyFailing(MaxRetriedTests + 1),
			wantExit:    1,
			wantFailed:  MaxRetriedTests + 1,
			wantAfter:   false,
			wantOutput:  fmt.Sprintf("%d failing tests not retried: a run retries at most %d in all", MaxRetriedTests+1, MaxRetriedTests),
			wantRetried: nil,
		},
		{
			name: "a failing subtest and its parent are one test, retried once",
			step: Step{Retries: 1},
			first: []string{ev("run", "TestParent"), ev("run", "TestParent/sub"), ev("fail", "TestParent/sub"),
				ev("run", "TestParent/other"), ev("pass", "TestParent/other"), ev("fail", "TestParent")},
			retry: func([]string, int) []string {
				return []string{ev("run", "TestParent"), ev("run", "TestParent/sub"), ev("pass", "TestParent/sub"), ev("pass", "TestParent")}
			},
			wantExit:    0,
			wantActions: map[string]string{"TestParent": "flaky", "TestParent/sub": "flaky", "TestParent/other": "pass"},
			wantRetries: map[string]int{"TestParent": 1, "TestParent/sub": 1},
			wantFlaky:   2,
			wantRetried: [][]string{{"TestParent"}},
			wantAfter:   true,
		},
		{
			name:        "a test that panicked is not retried: the tests after it never ran",
			step:        Step{Retries: 1},
			first:       []string{ev("run", "TestPanics"), ev("output", "TestPanics", "panic: nil map\n"), ev("fail", "TestPanics")},
			wantExit:    1,
			wantActions: map[string]string{"TestPanics": "fail"},
			wantFailed:  1,
			wantAfter:   false,
			wantOutput:  "not retried: pkg.TestPanics panicked",
		},
		{
			name:        "a package red with no failing test is not cleared by a flaky one elsewhere",
			step:        Step{Retries: 1},
			first:       cat(failing("TestFlaky"), []string{evIn("pkg", "fail", ""), evIn("other", "fail", "")}),
			wantExit:    1,
			wantActions: map[string]string{"TestFlaky": "fail"},
			wantFailed:  1,
			wantAfter:   false,
			wantOutput:  "package other failed with no failing test",
		},
		{
			name:        "a retry that reports nothing of the test confirms it",
			step:        Step{Retries: 1},
			first:       failing("TestBroken"),
			retry:       func([]string, int) []string { return nil },
			wantExit:    1,
			wantActions: map[string]string{"TestBroken": "fail"},
			wantRetries: map[string]int{"TestBroken": 1},
			wantFailed:  1,
			wantRetried: [][]string{{"TestBroken"}},
			wantAfter:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &supervisedRunner{script: func(c call, nth int) []string {
				switch {
				case strings.HasSuffix(c.cmd, "after"):
					return passing("TestAfter")
				case c.retry:
					return tc.retry(c.names, nth)
				}
				return tc.first
			}}
			unit := tc.step
			unit.Name, unit.Command, unit.Timeout = "unit", "go test", time.Minute
			steps := []Step{unit, {Name: "after", Command: "after", Timeout: time.Minute}}
			results, err := Run(context.Background(), steps, runner, Options{ContinueOnFailure: true})
			if err != nil {
				t.Fatal(err)
			}
			got := results[0]
			if got.ExitCode != tc.wantExit || got.TestsFlaky != tc.wantFlaky || got.TestsFailed != tc.wantFailed {
				t.Fatalf("unit exit %d, %d flaky, %d failed; want exit %d, %d flaky, %d failed\n%s",
					got.ExitCode, got.TestsFlaky, got.TestsFailed, tc.wantExit, tc.wantFlaky, tc.wantFailed, got.Output)
			}
			if tc.wantActions != nil {
				if a := actionsOf(got.Tests); fmt.Sprint(a) != fmt.Sprint(tc.wantActions) {
					t.Fatalf("tests %v, want %v", a, tc.wantActions)
				}
				want := tc.wantRetries
				if want == nil {
					want = map[string]int{}
				}
				if r := retriesOf(got.Tests); fmt.Sprint(r) != fmt.Sprint(want) {
					t.Fatalf("retries %v, want %v", r, want)
				}
			}
			var retried [][]string
			for _, c := range runner.retried() {
				retried = append(retried, c.names)
				// the unit's own command, told which tests: never another
				if !strings.HasSuffix(c.cmd, "; go test") {
					t.Fatalf("retry ran %q, want the unit's own command under the selection", c.cmd)
				}
				if c.timeout <= 0 || c.timeout >= time.Minute {
					t.Fatalf("retry was given %s, want what is left of the step's 1m", c.timeout)
				}
			}
			if fmt.Sprint(retried) != fmt.Sprint(tc.wantRetried) {
				t.Fatalf("retried %v, want %v", retried, tc.wantRetried)
			}
			if after := len(results) == 2; after != tc.wantAfter {
				t.Fatalf("the suite after ran = %v, want %v (results %d)", after, tc.wantAfter, len(results))
			}
			if !strings.Contains(got.Output, tc.wantOutput) {
				t.Fatalf("output %q, want it to say %q", got.Output, tc.wantOutput)
			}
		})
	}
}

// A test that passes on retry is flaky: its own Class and the unit's
// agree, and the unit is green.
func TestAFlakyTestsClassIsFlaky(t *testing.T) {
	runner := &supervisedRunner{script: func(c call, _ int) []string {
		if c.retry {
			return passing("TestFlaky")
		}
		return failing("TestFlaky")
	}}
	steps := []Step{{Name: "unit", Command: "go test", Timeout: time.Minute, Retries: 1}}
	results, err := Run(context.Background(), steps, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := results[0]
	if got.ExitCode != 0 {
		t.Fatalf("exit %d, want 0: a test flaky on retry is green", got.ExitCode)
	}
	if got.Class != ClassFlaky {
		t.Fatalf("step class %q, want %q", got.Class, ClassFlaky)
	}
	if len(got.Tests) != 1 || got.Tests[0].Action != ActionFlaky || got.Tests[0].Class != ClassFlaky {
		t.Fatalf("tests %+v, want TestFlaky flaky/%s", got.Tests, ClassFlaky)
	}
}

// A test that fails again on retry is real: the unit stays red and its
// Class says the failure stood.
func TestARealFailuresClassIsReal(t *testing.T) {
	runner := &supervisedRunner{script: func(call, int) []string { return failing("TestBroken") }}
	steps := []Step{{Name: "unit", Command: "go test", Timeout: time.Minute, Retries: 1}}
	results, err := Run(context.Background(), steps, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := results[0]
	if got.ExitCode == 0 {
		t.Fatal("exit 0, want red: the retry failed again")
	}
	if got.Class != ClassReal {
		t.Fatalf("step class %q, want %q", got.Class, ClassReal)
	}
	if len(got.Tests) != 1 || got.Tests[0].Action != "fail" || got.Tests[0].Class != ClassReal {
		t.Fatalf("tests %+v, want TestBroken fail/%s", got.Tests, ClassReal)
	}
}

// A retry happens after its unit has ended, never inside the unit's
// stream: while shard 0's retry is held, shard 1's tests go on reaching
// OnTest, and the retry's own events never do. A confirmed failure then
// stops the shard still running, which is named not run.
func TestARetryRunsAfterItsUnitEndsWhileTheOtherShardsStream(t *testing.T) {
	retryBegan := make(chan struct{})
	releaseRetry := make(chan struct{})
	shardOneSeen := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	runner := &supervisedRunner{}
	runner.hold = func(ctx context.Context, c call) {
		shard := shardIndexIn(c.cmd)
		switch {
		case c.retry:
			// the unit's own stream was read to its end before its
			// retry began
			mu.Lock()
			if !slices.Contains(seen, "TestBroken:fail") {
				t.Errorf("shard 0's retry began before its own run's failure was read: OnTest saw %v", seen)
			}
			mu.Unlock()
			close(retryBegan)
			<-releaseRetry
		case shard == "1":
			// shard 1 writes only once shard 0's retry is under way
			<-retryBegan
		}
	}
	runner.script = func(c call, _ int) []string {
		if c.retry || shardIndexIn(c.cmd) == "0" {
			return failing("TestBroken")
		}
		return passing("TestShardOne")
	}
	// shard 1 stays running until the confirmed failure stops it
	blocking := &blockAfter{supervisedRunner: runner}
	onTest := func(tr TestResult) {
		mu.Lock()
		seen = append(seen, tr.Name+":"+tr.Action)
		mu.Unlock()
		if tr.Name == "TestShardOne" {
			close(shardOneSeen)
		}
	}
	done := make(chan []StepResult, 1)
	go func() {
		steps := []Step{{Name: "e2e", Command: "go test", Shards: 2, Timeout: time.Minute, Retries: 1}}
		results, _ := Run(context.Background(), steps, blocking, Options{ContinueOnFailure: true, OnTest: onTest})
		done <- results
	}()
	select {
	case <-shardOneSeen:
		// shard 1's stream drained while shard 0's retry was held
	case <-time.After(5 * time.Second):
		t.Fatal("shard 1's test never reached OnTest while shard 0's retry ran")
	}
	close(releaseRetry)
	var results []StepResult
	select {
	case results = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never ended: the confirmed failure did not stop shard 1")
	}
	if len(results) != 1 || results[0].ExitCode == 0 {
		t.Fatalf("results %+v, want the one step red", results)
	}
	if len(results[0].Shards) != 1 || results[0].Shards[0].Index != 0 {
		t.Fatalf("shards %+v, want shard 0 alone: shard 1 was stopped by the confirmed failure, not run", results[0].Shards)
	}
	if !strings.Contains(results[0].Output, "shard 2/2: not run") {
		t.Fatalf("output %q, want shard 2 named not run", results[0].Output)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := len(seen); n != 2 || !slices.Contains(seen, "TestBroken:fail") || !slices.Contains(seen, "TestShardOne:pass") {
		t.Fatalf("OnTest saw %v, want each shard's own test once and nothing of the retry", seen)
	}
}

// blockAfter is a supervisedRunner whose shard 1 stays running after it
// has written its lines, until its ctx ends: a shard still going when
// another's failure is confirmed.
type blockAfter struct{ *supervisedRunner }

func (b *blockAfter) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	exit, err := b.supervisedRunner.Run(ctx, cmd, timeout, stdout, stderr)
	if shardIndexIn(cmd) == "1" && !strings.Contains(cmd, SuiteTestsEnv) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	return exit, err
}

func (b *blockAfter) Lane(context.Context, string) (Runner, func(), error) {
	return b, func() {}, nil
}

// A run retries MaxRetriedTests failing tests in all, over every unit:
// two shards with six each are twelve, so one shard's are retried and
// the other's are not.
func TestTheRetryCapIsTheRunsNotAUnits(t *testing.T) {
	runner := &supervisedRunner{script: func(c call, _ int) []string {
		if c.retry {
			var out []string
			for _, n := range c.names {
				out = append(out, passing(n)...)
			}
			return out
		}
		return manyFailing(6)
	}}
	steps := []Step{{Name: "e2e", Command: "go test", Shards: 2, Timeout: time.Minute, Retries: 1, RunToEnd: true}}
	results, err := Run(context.Background(), steps, runner, Options{ContinueOnFailure: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(runner.retried()); n != 1 {
		t.Fatalf("%d retries, want 1: the second shard's six are past the run's cap", n)
	}
	if results[0].TestsFlaky != 6 || results[0].TestsFailed != 6 || results[0].ExitCode == 0 {
		t.Fatalf("step %d flaky, %d failed, exit %d; want 6 flaky, 6 failed, red", results[0].TestsFlaky, results[0].TestsFailed, results[0].ExitCode)
	}
}

// A test that started and then went silent past its suite's stall limit
// is named stalled and its unit stopped, long before the step's
// timeout. A parent waiting on its subtest and a test paused for its
// parallel siblings are not the stalled ones.
func TestAStalledTestIsNamedAndItsUnitStopped(t *testing.T) {
	runner := &supervisedRunner{script: func(call, int) []string {
		return cat(passing("TestOK"), []string{
			ev("run", "TestParent"), ev("run", "TestParent/hangs"),
			ev("run", "TestPaused"), ev("pause", "TestPaused"),
		})
	}}
	hung := &hangs{supervisedRunner: runner}
	steps := []Step{
		{Name: "unit", Command: "go test", Timeout: time.Minute, Stall: 40 * time.Millisecond, Retries: 1},
		{Name: "after", Command: "after", Timeout: time.Minute},
	}
	start := time.Now()
	results, err := Run(context.Background(), steps, hung, Options{ContinueOnFailure: true})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the run took %s: the stall did not stop the unit", took)
	}
	got := results[0]
	if !got.Stopped || got.ExitCode == 0 {
		t.Fatalf("unit %+v, want it stopped and red", got)
	}
	if want := "stalled: pkg.TestParent/hangs produced no event for 40ms (validate.yml stall:)"; got.StopReason != want {
		t.Fatalf("stop reason %q, want %q", got.StopReason, want)
	}
	want := map[string]string{"TestOK": "pass", "TestParent": "timeout", "TestParent/hangs": "stalled", "TestPaused": "timeout"}
	if a := actionsOf(got.Tests); fmt.Sprint(a) != fmt.Sprint(want) {
		t.Fatalf("tests %v, want %v", a, want)
	}
	if len(runner.retried()) != 0 {
		t.Fatal("a stopped unit was retried")
	}
}

// A stalled unit is infrastructure, not a confirmed failure: the
// environment stopped it before its code could be judged, so it is
// never retried.
func TestAStalledUnitsClassIsInfrastructure(t *testing.T) {
	runner := &supervisedRunner{script: func(call, int) []string {
		return []string{ev("run", "TestHangs")}
	}}
	hung := &hangs{supervisedRunner: runner}
	steps := []Step{{Name: "unit", Command: "go test", Timeout: time.Minute, Stall: 40 * time.Millisecond, Retries: 1}}
	results, err := Run(context.Background(), steps, hung, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := results[0]
	if got.ExitCode == 0 {
		t.Fatal("exit 0, want red: the stall stopped the unit")
	}
	if got.Class != ClassInfrastructure {
		t.Fatalf("step class %q, want %q", got.Class, ClassInfrastructure)
	}
	if len(runner.retried()) != 0 {
		t.Fatal("a stalled unit was retried: it is infrastructure, not a confirmed failure")
	}
}

// hangs is a supervisedRunner whose command never ends on its own once
// it has written its lines.
type hangs struct{ *supervisedRunner }

func (h *hangs) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	if cmd == "after" {
		return h.supervisedRunner.Run(ctx, cmd, timeout, stdout, stderr)
	}
	h.supervisedRunner.Run(ctx, cmd, timeout, stdout, stderr)
	<-ctx.Done()
	return -1, ctx.Err()
}

// The stall is the unit's silence, not one test's: while any test of
// the unit reports, none is stalled, however long one of them has been
// quiet; once the whole stream has been silent for the limit, the tests
// still running are — a parent waiting on its subtest and a test paused
// for its parallel siblings not among them — and a package whose binary
// has ended leaves no stalled test behind.
func TestStallWatchReadsWhoIsRunning(t *testing.T) {
	now := time.Unix(0, 0)
	w := newStallWatch(time.Minute, func() time.Time { return now })
	write := func(lines ...string) {
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	}
	names := func() string {
		var out []string
		for _, k := range w.check() {
			out = append(out, k.String())
		}
		return strings.Join(out, ",")
	}
	write(ev("run", "TestChatty"), ev("run", "TestQuiet"), evIn("dead", "run", "TestLeftOpen"))
	now = now.Add(50 * time.Second)
	write(ev("output", "TestChatty", "still going\n"), evIn("dead", "fail", ""))
	now = now.Add(20 * time.Second)
	// TestQuiet has been silent 70s, but the unit wrote 20s ago
	if got := names(); got != "" {
		t.Fatalf("stalled %q while the unit still reports, want none", got)
	}
	now = now.Add(40 * time.Second)
	// the whole stream silent 60s: both still running are stalled,
	// TestLeftOpen's package having ended
	if got := names(); got != "pkg.TestChatty,pkg.TestQuiet" {
		t.Fatalf("stalled %q, want pkg.TestChatty,pkg.TestQuiet", got)
	}
	write(ev("pass", "TestQuiet"), ev("pass", "TestChatty"), "not json",
		ev("run", "TestParent"), ev("run", "TestParent/sub"), ev("run", "TestPaused"), ev("pause", "TestPaused"))
	now = now.Add(59 * time.Second)
	if got := names(); got != "" {
		t.Fatalf("stalled %q under the limit, want none", got)
	}
	now = now.Add(time.Second)
	if got := names(); got != "pkg.TestParent/sub" {
		t.Fatalf("stalled %q, want pkg.TestParent/sub alone", got)
	}
}

// A suite that queues its own tests is not stalled while it progresses
// (2026-11-16, every-hung-test-is-found-and-fixed-by-its-cause):
// runsheet's e2e harness lets every test of a shard through t.Parallel
// at once — go test's run, pause and cont for each — and then holds
// each on its own running slots, six of them, until one comes free. A
// test at the back of that queue is silent, and "running" to go test,
// for most of the shard; watched test by test, every one still queued
// at the limit was named stalled and its shard stopped. Here 30 tests
// queue for 2 slots, each running 3 minutes: the last waits 42 minutes,
// far past the 5-minute limit, while the stream moves every 3. Only when
// the slots' holders stop reporting too is the unit stalled.
func TestTestsQueuedBehindTheSuitesOwnSlotsAreNotStalled(t *testing.T) {
	now := time.Unix(0, 0)
	w := newStallWatch(5*time.Minute, func() time.Time { return now })
	write := func(lines ...string) {
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	}
	const tests, slots = 30, 2
	name := func(i int) string { return fmt.Sprintf("TestRig%02d", i) }
	for i := range tests {
		write(ev("run", name(i)), ev("pause", name(i)))
	}
	for i := range tests {
		write(ev("cont", name(i)))
	}
	for i := 0; i < tests; i += slots {
		for j := i; j < i+slots; j++ {
			write(ev("output", name(j), "rig up\n"))
		}
		now = now.Add(3 * time.Minute)
		if got := w.check(); len(got) > 0 {
			t.Fatalf("%s in: %d tests named stalled while two hold the slots and report: %v", now.Sub(time.Unix(0, 0)), len(got), got)
		}
		for j := i; j < i+slots; j++ {
			write(ev("pass", name(j)))
		}
	}
	// a last test takes a slot and hangs: the stream goes quiet, and it
	// is named once the stream has been quiet the limit
	write(ev("run", "TestHangs"), ev("pause", "TestHangs"), ev("cont", "TestHangs"), ev("output", "TestHangs", "rig up\n"))
	now = now.Add(5 * time.Minute)
	if got := fmt.Sprint(w.check()); got != "[pkg.TestHangs]" {
		t.Fatalf("stalled %s, want [pkg.TestHangs]", got)
	}
}

// A slow suite that progresses runs to its end (2026-11-16): a unit
// whose tests queue for the suite's own slots, as runsheet's e2e shards
// do, and take far longer in all than the stall limit — every queued
// test silent for longer than the limit — is not stopped, and its tests
// pass.
func TestASlowButProgressingSuiteIsNotStalled(t *testing.T) {
	const tests, slots, each = 24, 2, 60 * time.Millisecond
	runner := &queueRunner{tests: tests, slots: slots, each: each}
	steps := []Step{{Name: "integration", Command: "go test -json ./e2e", Timeout: time.Minute, Stall: 250 * time.Millisecond}}
	start := time.Now()
	results, err := Run(context.Background(), steps, runner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	got := results[0]
	if got.Stopped || got.ExitCode != 0 || got.TestsPassed != tests {
		t.Fatalf("unit stopped %v after %s (%.120q), exit %d, %d of %d passed; want every test passed and no stop", got.Stopped, took, got.StopReason, got.ExitCode, got.TestsPassed, tests)
	}
	if took < 2*steps[0].Stall {
		t.Fatalf("the run took %s, want well past the %s limit for the test to mean anything", took, steps[0].Stall)
	}
}

// queueRunner is a go test -json run of tests parallel tests let through
// t.Parallel at once and run slots at a time by the suite's own queue,
// each for each, as runsheet's e2e harness does: a test is silent from
// its cont until its slot.
type queueRunner struct {
	tests, slots int
	each         time.Duration
}

func (q *queueRunner) Run(ctx context.Context, _ string, _ time.Duration, stdout, _ io.Writer) (int, error) {
	var mu sync.Mutex
	emit := func(action, test string, output ...string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintln(stdout, ev(action, test, output...))
	}
	name := func(i int) string { return fmt.Sprintf("TestRig%02d", i) }
	for i := range q.tests {
		emit("run", name(i))
		emit("pause", name(i))
	}
	for i := range q.tests {
		emit("cont", name(i))
	}
	slot := make(chan struct{}, q.slots)
	var wg sync.WaitGroup
	for i := range q.tests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slot <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slot }()
			emit("output", name(i), "rig up\n")
			select {
			case <-time.After(q.each):
				emit("pass", name(i))
			case <-ctx.Done():
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	emit("pass", "")
	return 0, nil
}

const floorYML = `suites:
  - name: %s
    kind: %s
    command: go test
%s`

func parsedStep(t *testing.T, kind, extra string) Step {
	t.Helper()
	cfg, err := Parse([]byte(fmt.Sprintf(floorYML, "suite", kind, extra)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Steps[0]
}

// "0 tests" is never green where the suite has a floor: a unit or
// integration suite's is 1 unless the file says otherwise, held where
// the node could count the tests; min_tests states one outright.
func TestAUnitThatRanNoTestsIsRed(t *testing.T) {
	noTests := []string{evIn("pkg", "start", ""), evIn("pkg", "output", "", "ok  \tpkg\t0.1s [no tests to run]\n"), evIn("pkg", "pass", "")}
	cases := []struct {
		name       string
		step       Step
		lines      []string
		ctx        func(context.Context) context.Context
		wantExit   int
		wantReason string
	}{
		{name: "a unit suite whose go test stream names no test", step: parsedStep(t, "unit", ""), lines: noTests, wantExit: 1, wantReason: "ran 0 tests"},
		{name: "an integration suite the same", step: parsedStep(t, "integration", ""), lines: noTests, wantExit: 1, wantReason: "ran 0 tests"},
		{name: "every test skipped is still tests run", step: parsedStep(t, "unit", ""), lines: []string{ev("run", "TestDB"), ev("skip", "TestDB")}},
		{name: "a lint suite has no floor", step: parsedStep(t, "lint", ""), lines: noTests},
		{name: "a unit suite the node cannot count is not held to the implied floor", step: parsedStep(t, "unit", ""), lines: []string{"ok, 12 specs"}},
		{name: "a stated floor holds whatever the output", step: parsedStep(t, "unit", "    min_tests: 1\n"), lines: []string{"ok, 12 specs"}, wantExit: 1, wantReason: "ran 0 tests"},
		{name: "a stated floor on any kind", step: parsedStep(t, "build", "    min_tests: 1\n"), lines: noTests, wantExit: 1, wantReason: "ran 0 tests"},
		{name: "a floor above what ran", step: parsedStep(t, "unit", "    min_tests: 3\n"), lines: passing("TestOne"), wantExit: 1, wantReason: "ran 1 tests, fewer than min_tests 3"},
		{name: "min_tests 0 turns the floor off", step: parsedStep(t, "unit", "    min_tests: 0\n"), lines: noTests},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &supervisedRunner{script: func(call, int) []string { return tc.lines }}
			results, err := Run(context.Background(), []Step{tc.step}, runner, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if results[0].ExitCode != tc.wantExit || results[0].Reason != tc.wantReason {
				t.Fatalf("exit %d, reason %q; want exit %d, reason %q", results[0].ExitCode, results[0].Reason, tc.wantExit, tc.wantReason)
			}
			if tc.wantReason != "" && !strings.Contains(results[0].Output, "supervisor: "+tc.wantReason) {
				t.Fatalf("output %q, want the reason", results[0].Output)
			}
		})
	}
}

// Under a call's selection a shard may rightly run nothing: the floor
// is then the step's shards' together, and still red when none ran any.
func TestTheFloorUnderASelectionIsTheStepsNotAShards(t *testing.T) {
	step := parsedStep(t, "integration", "    shards: 2\n")
	empty := []string{evIn("pkg", "pass", "")}
	for _, tc := range []struct {
		name     string
		shardOne []string
		selected bool
		wantExit int
	}{
		{name: "selected, one shard holds the test", shardOne: passing("TestPicked"), selected: true},
		{name: "selected, no shard ran any", shardOne: empty, selected: true, wantExit: 1},
		{name: "the whole suite, an empty shard is red", shardOne: passing("TestPicked"), wantExit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &supervisedRunner{script: func(c call, _ int) []string {
				if shardIndexIn(c.cmd) == "1" {
					return tc.shardOne
				}
				return empty
			}}
			ctx := context.Background()
			if tc.selected {
				ctx = WithSelection(ctx)
			}
			results, err := Run(ctx, []Step{step}, runner, Options{ContinueOnFailure: true})
			if err != nil {
				t.Fatal(err)
			}
			if results[0].ExitCode != tc.wantExit {
				t.Fatalf("exit %d, want %d (%+v)", results[0].ExitCode, tc.wantExit, results[0])
			}
			if tc.wantExit != 0 && results[0].Reason != "ran 0 tests" {
				t.Fatalf("reason %q, want ran 0 tests", results[0].Reason)
			}
		})
	}
}

// The supervisor's keys are policy, read from the suite's own entry:
// their defaults, their bounds, and none of them in the suite hash.
func TestParseReadsTheSupervisorsPolicy(t *testing.T) {
	def := parsedStep(t, "unit", "")
	if def.Retries != 1 || def.RunToEnd || def.Stall != 5*time.Minute || def.MinTests != 1 {
		t.Fatalf("defaults %+v, want retries 1, stall 5m, min_tests 1", def)
	}
	if lint := parsedStep(t, "lint", ""); lint.MinTests != 0 {
		t.Fatalf("a lint suite's min_tests = %d, want 0", lint.MinTests)
	}
	set := parsedStep(t, "integration", "    retries: 0\n    run_to_end: true\n    stall: 90s\n    min_tests: 40\n")
	if set.Retries != 0 || !set.RunToEnd || set.Stall != 90*time.Second || set.MinTests != 40 {
		t.Fatalf("set %+v, want retries 0, run_to_end, stall 90s, min_tests 40", set)
	}
	for _, off := range []string{"0", "off"} {
		if st := parsedStep(t, "unit", "    stall: "+off+"\n"); st.Stall != 0 {
			t.Fatalf("stall %s = %s, want none", off, st.Stall)
		}
	}
	for _, bad := range []string{"retries: -1", "retries: 6", "stall: soon", "stall: -1m", "min_tests: -1"} {
		if _, err := Parse([]byte(fmt.Sprintf(floorYML, "suite", "unit", "    "+bad+"\n"))); err == nil {
			t.Fatalf("%s parsed, want it refused", bad)
		}
	}
	plain := mustHash(t, fmt.Sprintf(floorYML, "suite", "unit", ""))
	policy := mustHash(t, fmt.Sprintf(floorYML, "suite", "unit", "    retries: 2\n    run_to_end: true\n    stall: 1m\n    min_tests: 5\n"))
	if plain != policy {
		t.Fatal("the supervisor's policy changed the suite hash")
	}
}
