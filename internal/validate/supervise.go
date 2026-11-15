package validate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Run supervises the tests its units run, so the session that asked for
// the run never has to (ADR-049's amendment of 2026-11-15). Four things,
// each a suite's policy in validate.yml and none a command of its own:
//
//   - retries: a unit that ends red on failing tests has those tests —
//     the top-level ones, a failing subtest's parent named once — run
//     again by the node, with the unit's own command under
//     MESH_SUITE_TESTS (SuiteTestsEnv), after the unit has ended, in the
//     unit's own lane and inside what is left of its step's timeout. One
//     that passes is flaky and no longer fails the unit; one that fails
//     again is a confirmed failure.
//   - run_to_end: a confirmed failure stops the run as fail_fast does,
//     unless the suite says run_to_end.
//   - stall: a started test with no event for the limit is named
//     stalled and its unit stopped, by the unit's own stop func
//     (Options.OnLane's, stop_suite's).
//   - min_tests: a unit that exits 0 having reported fewer tests than
//     the floor is red, "ran 0 tests".

// SuiteTestsEnv names the tests a suite's command is to run, space
// separated: run_suite's own selection (internal/nodetools) and the
// supervisor's retry both set it, so a suite has one way to be told
// which tests to run.
const SuiteTestsEnv = "MESH_SUITE_TESTS"

// The Actions the supervisor writes on a TestResult, beside go test's
// own pass, fail and skip and the parser's timeout.
const (
	ActionFlaky   = "flaky"
	ActionStalled = "stalled"
)

type selectionKey struct{}

// WithSelection is ctx saying the run's commands were told to run only
// part of their suites (run_suite's targets and tests): a shard the
// selection leaves empty is then no "ran 0 tests", and a suite's
// min_tests is held against the step's shards together.
func WithSelection(ctx context.Context) context.Context {
	return context.WithValue(ctx, selectionKey{}, true)
}

// SelectionAsked says whether ctx carries WithSelection.
func SelectionAsked(ctx context.Context) bool {
	v, _ := ctx.Value(selectionKey{}).(bool)
	return v
}

// retryBudget is how many more failing tests the run retries
// (MaxRetriedTests in all, over every unit).
type retryBudget struct {
	mu   sync.Mutex
	left int
}

// take takes n of what is left, all or nothing.
func (b *retryBudget) take(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.left {
		return false
	}
	b.left -= n
	return true
}

// testKey names one test of one package.
type testKey struct{ pkg, name string }

func (k testKey) String() string {
	if k.pkg == "" {
		return k.name
	}
	return k.pkg + "." + k.name
}

// topLevel is the test a (sub)test belongs to: its name up to the first
// slash, which is what a suite's command can be asked to run by name.
func topLevel(name string) string {
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[:i]
	}
	return name
}

// failedTopLevel are the top-level tests behind tests' failures, each
// once, in the order they failed: a failing subtest and its parent are
// one test to run again.
func failedTopLevel(tests []TestResult) []testKey {
	var out []testKey
	seen := map[testKey]bool{}
	for _, t := range tests {
		if t.Action != "fail" {
			continue
		}
		k := testKey{t.Package, topLevel(t.Name)}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// retryCommand is cmd told to run only names: the selection variable
// exported before it, as a shard's index is (shardCommand).
func retryCommand(cmd string, names []string) string {
	return fmt.Sprintf("export %s='%s'; %s", SuiteTestsEnv, strings.ReplaceAll(strings.Join(names, " "), "'", `'\''`), cmd)
}

// retryNames are the names keys' tests are run again by: each test's
// name once, whatever its package, in order.
func retryNames(keys []testKey) []string {
	var out []string
	for _, k := range keys {
		if !slices.Contains(out, k.name) {
			out = append(out, k.name)
		}
	}
	return out
}

// goTestEvent is one line of go test -json, as the supervisor reads it.
type goTestEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

// scanGoTest calls fn with each go test -json event in raw.
func scanGoTest(raw []byte, fn func(goTestEvent) bool) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev goTestEvent
		if json.Unmarshal(line, &ev) != nil || ev.Action == "" {
			continue
		}
		if !fn(ev) {
			return
		}
	}
}

// isGoTestStream says whether raw carries any go test -json event: the
// node can then count the unit's tests, none included.
func isGoTestStream(raw []byte) bool {
	found := false
	scanGoTest(raw, func(ev goTestEvent) bool {
		found = ev.Package != "" || ev.Test != ""
		return !found
	})
	return found
}

// failedPackages are the packages raw's stream ends in fail.
func failedPackages(raw []byte) []string {
	var out []string
	scanGoTest(raw, func(ev goTestEvent) bool {
		if ev.Action == "fail" && ev.Test == "" && ev.Package != "" && !slices.Contains(out, ev.Package) {
			out = append(out, ev.Package)
		}
		return true
	})
	return out
}

// resultsStream is the go test -json stream step's results name: its
// stdout, or its results file; nil for a JUnit report.
func resultsStream(dir string, step Step, stdout []byte) []byte {
	switch {
	case strings.HasPrefix(step.Results, "go-test-json:"):
		raw, _ := os.ReadFile(filepath.Join(dir, strings.TrimPrefix(step.Results, "go-test-json:")))
		return raw
	case strings.HasPrefix(step.Results, "junit:"):
		return nil
	}
	return stdout
}

// notRetried is why a red unit's failing tests are taken as they are,
// with no retry: "" when a retry could clear them. A retry is only a
// fair second hearing where the failing tests are the whole of the
// red: a package that failed with no failing test (it did not build, or
// its binary died), a test left running, a test that panicked (taking
// the tests after it down unrun) are failures no re-run of the named
// tests answers for.
func notRetried(failed []testKey, tests []TestResult, stream []byte) string {
	for _, k := range failed {
		if k.name == "" || strings.ContainsFunc(k.name, unicode.IsSpace) {
			return fmt.Sprintf("test %q cannot be named to the suite's command", k.name)
		}
	}
	for _, t := range tests {
		switch {
		case t.Action == "timeout":
			return fmt.Sprintf("%s never finished", testKey{t.Package, t.Name})
		case t.Action == "fail" && (strings.HasPrefix(t.Output, "panic: ") || strings.Contains(t.Output, "\npanic: ")):
			return fmt.Sprintf("%s panicked", testKey{t.Package, t.Name})
		}
	}
	for _, pkg := range failedPackages(stream) {
		if !slices.ContainsFunc(failed, func(k testKey) bool { return k.pkg == pkg }) {
			return fmt.Sprintf("package %s failed with no failing test", pkg)
		}
	}
	return ""
}

// supervisor is one Run's supervision of its units.
type supervisor struct {
	dir      string
	budget   *retryBudget
	onTest   func(TestResult)
	selected bool
}

// command runs cmd in r for a unit, its stdout kept on out (the first
// run's) or discarded into into (a retry's), streamed to onTest when
// live, and watched for a stalled test: the unit's own stop func ends it
// then, and the stalled tests are answered.
func (s *supervisor) command(ctx context.Context, r Runner, cmd string, timeout, stall time.Duration, ls *unitStop, live bool, stdout, stderr *bytes.Buffer) (exit int, err error, stalled []testKey) {
	ws := []io.Writer{stdout}
	if live && s.onTest != nil {
		ws = append(ws, newTestEventWriter(s.onTest))
	}
	var watch *stallWatch
	stop := func() {}
	if stall > 0 {
		watch = newStallWatch(stall, time.Now)
		ws = append(ws, watch)
		stop = watch.start(func(names []testKey) {
			ls.stop(stallReason(names, stall))
		})
	}
	w := ws[0]
	if len(ws) > 1 {
		w = io.MultiWriter(ws...)
	}
	exit, err = r.Run(ctx, cmd, timeout, w, stderr)
	stop()
	if watch != nil {
		stalled = watch.fired()
	}
	return exit, err, stalled
}

// stallReason is a stalled unit's StopReason.
func stallReason(names []testKey, limit time.Duration) string {
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = k.String()
	}
	return fmt.Sprintf("stalled: %s produced no event for %s (validate.yml stall:)", strings.Join(parts, ", "), limit)
}

// parse reads the unit's results once its command has ended: its
// counts, tests and report are the unit's from here on, whatever a
// retry then writes over the same results file.
func (s *supervisor) parse(step Step, out *ran) {
	out.run, out.passed, out.failed, out.report, out.tests, out.parseErr = parseResults(s.dir, step, out.stdout.Bytes())
	out.counted = step.Results != "" || isGoTestStream(out.stdout.Bytes())
	if len(out.stalled) == 0 {
		return
	}
	for i, t := range out.tests {
		if t.Action == "timeout" && slices.Contains(out.stalled, testKey{t.Package, t.Name}) {
			out.tests[i].Action = ActionStalled
		}
	}
}

// retry gives a red unit's failing tests their second hearing: step is
// the unit's (a shard's own, its results path its own), cmd its command,
// started when it began. It runs after the unit's command has ended,
// in the unit's lane r and on its ctx (so stop_suite and fail fast still
// end it), never more than step.Retries times and never past what is
// left of step.Timeout. out is rewritten: a test that passed is flaky,
// one that did not carries its Retries, and a unit whose every failure
// was flaky exits 0. out.confirmed says a failure stood.
func (s *supervisor) retry(ctx context.Context, r Runner, step Step, cmd string, started time.Time, ls *unitStop, out *ran) {
	if step.Retries <= 0 || out.stopped || out.runErr != nil || out.parseErr != nil || out.exitCode == 0 || out.exitCode == 124 {
		return
	}
	failed := failedTopLevel(out.tests)
	if len(failed) == 0 {
		// red with no failing test named: nothing to run again, and
		// nothing the supervisor can call confirmed
		return
	}
	note := func(format string, args ...any) {
		fmt.Fprintf(&out.notes, "supervisor: "+format+"\n", args...)
	}
	if why := notRetried(failed, out.tests, resultsStream(s.dir, step, out.stdout.Bytes())); why != "" {
		out.confirmed = true
		note("%d failing tests not retried: %s", len(failed), why)
		return
	}
	if !s.budget.take(len(failed)) {
		out.confirmed = true
		note("%d failing tests not retried: a run retries at most %d in all", len(failed), MaxRetriedTests)
		return
	}
	tries := map[testKey]int{}
	flaky := map[testKey]bool{}
	left := failed
	for attempt := 1; attempt <= step.Retries && len(left) > 0; attempt++ {
		remaining := step.Timeout - time.Since(started)
		if step.Timeout <= 0 {
			remaining = DefaultTimeout - time.Since(started)
		}
		if remaining <= 0 {
			note("retry %d not run: the step's timeout of %s is spent", attempt, step.Timeout)
			break
		}
		if ctx.Err() != nil {
			break
		}
		names := retryNames(left)
		var stdout, stderr bytes.Buffer
		exit, err, _ := s.command(ctx, r, retryCommand(cmd, names), remaining, step.Stall, ls, false, &stdout, &stderr)
		if _, stopped := ls.stopped(); stopped || err != nil {
			note("retry %d of %s did not finish", attempt, strings.Join(names, " "))
			break
		}
		_, _, _, _, tests, perr := parseResults(s.dir, step, stdout.Bytes())
		if perr != nil {
			note("retry %d of %s left no results to read: %v", attempt, strings.Join(names, " "), perr)
			break
		}
		passed := map[testKey]bool{}
		for _, t := range tests {
			if t.Action == "pass" && t.Name == topLevel(t.Name) {
				passed[testKey{t.Package, t.Name}] = true
			}
		}
		var still []testKey
		for _, k := range left {
			tries[k] = attempt
			if passed[k] {
				flaky[k] = true
			} else {
				still = append(still, k)
			}
		}
		note("retry %d ran %s (exit %d): %d passed, %d failed again", attempt, strings.Join(names, " "), exit, len(left)-len(still), len(still))
		left = still
	}
	out.failed, out.flaky = 0, 0
	for i, t := range out.tests {
		if t.Action != "fail" {
			continue
		}
		k := testKey{t.Package, topLevel(t.Name)}
		out.tests[i].Retries = tries[k]
		if flaky[k] {
			out.tests[i].Action = ActionFlaky
			out.flaky++
		} else {
			out.failed++
		}
	}
	if len(flaky) == len(failed) {
		// every failure passed when run again: the unit is green, its
		// flaky tests named
		out.exitCode = 0
		return
	}
	if _, stopped := ls.stopped(); !stopped && ctx.Err() == nil {
		out.confirmed = true
	}
}

// floor holds a unit that exited 0 to its suite's min_tests: under a
// selection the step's shards are held together instead (stepFloor).
func (s *supervisor) floor(step Step, out *ran) {
	if s.selected || out.exitCode != 0 || out.runErr != nil || out.stopped || out.parseErr != nil {
		return
	}
	if reason := belowFloor(step, out.run, out.counted); reason != "" {
		out.exitCode = 1
		out.reason = reason
		fmt.Fprintf(&out.notes, "supervisor: %s: the suite's min_tests is %d\n", reason, step.MinTests)
	}
}

// belowFloor is the reason a unit (or, under a selection, a step) that
// reported run tests is under step's min_tests, "" when it is not. The
// kind's implied floor holds only where the tests were counted.
func belowFloor(step Step, run int, counted bool) string {
	if step.MinTests <= 0 || run >= step.MinTests || (step.minTestsImplied && !counted) {
		return ""
	}
	if run == 0 {
		return "ran 0 tests"
	}
	return fmt.Sprintf("ran %d tests, fewer than min_tests %d", run, step.MinTests)
}

// stallWatch reads a unit's go test -json stream as it arrives and
// knows which tests are running: started, not paused for a parallel
// sibling, with no subtest of their own still open. One of those silent
// for limit — no event of its own, nor of a subtest — is stalled.
type stallWatch struct {
	limit time.Duration
	now   func() time.Time

	mu      sync.Mutex
	buf     []byte
	open    map[testKey]*openTest
	stalled []testKey
}

type openTest struct {
	last   time.Time
	paused bool
}

func newStallWatch(limit time.Duration, now func() time.Time) *stallWatch {
	return &stallWatch{limit: limit, now: now, open: map[testKey]*openTest{}}
}

// Write only records: the watching is the ticker's (start), so the
// stream is never held while a stall is acted on.
func (w *stallWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.line(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *stallWatch) line(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var ev goTestEvent
	if json.Unmarshal(line, &ev) != nil || ev.Action == "" {
		return
	}
	if ev.Test == "" {
		switch ev.Action {
		case "pass", "fail", "skip":
			// the package's binary has ended: a test it left open is
			// its failure's, not a stall
			for k := range w.open {
				if k.pkg == ev.Package {
					delete(w.open, k)
				}
			}
		}
		return
	}
	now := w.now()
	k := testKey{ev.Package, ev.Test}
	switch ev.Action {
	case "run":
		w.open[k] = &openTest{last: now}
	case "pass", "fail", "skip":
		delete(w.open, k)
	case "pause":
		if t, ok := w.open[k]; ok {
			t.paused = true
		}
	case "cont":
		if t, ok := w.open[k]; ok {
			t.paused = false
		}
	}
	// an event is its test's and every parent's
	for name := ev.Test; ; {
		if t, ok := w.open[testKey{ev.Package, name}]; ok {
			t.last = now
		}
		i := strings.LastIndexByte(name, '/')
		if i < 0 {
			break
		}
		name = name[:i]
	}
}

// check are the tests stalled now, sorted.
func (w *stallWatch) check() []testKey {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	var out []testKey
	for k, t := range w.open {
		if t.paused || now.Sub(t.last) < w.limit {
			continue
		}
		parent := false
		for o := range w.open {
			if o.pkg == k.pkg && strings.HasPrefix(o.name, k.name+"/") {
				parent = true
				break
			}
		}
		if !parent {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// start watches until the returned func is called, calling onStall
// once, with the stalled tests, the first time any is.
func (w *stallWatch) start(onStall func([]testKey)) (stop func()) {
	done := make(chan struct{})
	ended := make(chan struct{})
	every := min(max(w.limit/4, 5*time.Millisecond), 5*time.Second)
	go func() {
		defer close(ended)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			if names := w.check(); len(names) > 0 {
				w.mu.Lock()
				w.stalled = names
				w.mu.Unlock()
				onStall(names)
				return
			}
		}
	}()
	return func() {
		close(done)
		<-ended
	}
}

// fired are the tests the watch stopped its unit for, nil when it did
// not.
func (w *stallWatch) fired() []testKey {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stalled
}
