package nodetools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/validate"
)

// RunSuiteTool is the run_suite node tool's name. It runs the
// checkout's declared validation (.runsheet/validate.yml, through
// internal/validate) — every suite, or the ones named — on the node, and
// records the result: its output blob is every step's outcome and every
// test's final event, go test -json normalised to {package, test,
// action, elapsed}.
const RunSuiteTool = "run_suite"

// SuiteRunner runs validate's steps in a workspace (validate.Run on a
// HostRunner in it), each step's environment also carrying env: the
// call's selection (SelectionEnv), empty when it named none.
type SuiteRunner func(ctx context.Context, workspace string, steps []validate.Step, env []string) ([]validate.StepResult, error)

// The variables a suite's command reads its selection from (projectmesh-io/api
// ADR 001 amendment, evidence waivers; mesh-specs gap 27): each space
// separated, and unset when the call named none. A command opts in, e.g.
// go test ${MESH_SUITE_TARGETS:-./...} ${MESH_SUITE_TESTS:+-run "^(${MESH_SUITE_TESTS// /|})$"};
// one that ignores them runs whole, which is still correct.
const (
	SuiteTargetsEnv = "MESH_SUITE_TARGETS"
	SuiteTestsEnv   = "MESH_SUITE_TESTS"
)

// HostSuites runs steps on the host, in the workspace, every step run
// whatever an earlier one did. The steps are the session's own, so they
// see none of the node's model variables (harness.ModelEnv; ADR 001
// amendment "model relay"), and no selection but the call's.
func HostSuites(ctx context.Context, workspace string, steps []validate.Step, env []string) ([]validate.StepResult, error) {
	var base []string
	for _, kv := range harness.EnvironWithoutModel() {
		if !strings.HasPrefix(kv, SuiteTargetsEnv+"=") && !strings.HasPrefix(kv, SuiteTestsEnv+"=") {
			base = append(base, kv)
		}
	}
	host := validate.HostRunner{Dir: workspace, Env: append(base, env...)}
	return validate.Run(ctx, steps, host, validate.Options{Dir: workspace, ContinueOnFailure: true})
}

// Selection is the part of a suite a run_suite call named: the targets
// (packages, paths or build targets) and the tests (test names). The
// signed record carries it exactly as named, either list omitted when
// empty; a call that named neither has none, and its record no
// selection key at all.
type Selection struct {
	Targets []string `json:"targets,omitempty"`
	Tests   []string `json:"tests,omitempty"`
}

// maxSelection is how many targets, and how many tests, a selection may
// name: Mesh refuses a record naming more.
const maxSelection = 500

// SelectionEnv is the selection as its suite's command reads it:
// MESH_SUITE_TARGETS and MESH_SUITE_TESTS, each space separated, each
// absent when the selection names none.
func SelectionEnv(s *Selection) []string {
	if s == nil {
		return nil
	}
	var env []string
	if len(s.Targets) > 0 {
		env = append(env, SuiteTargetsEnv+"="+strings.Join(s.Targets, " "))
	}
	if len(s.Tests) > 0 {
		env = append(env, SuiteTestsEnv+"="+strings.Join(s.Tests, " "))
	}
	return env
}

// ErrNoSuiteRunner is run_suite refused: the lease has nowhere to run
// its steps. The steps are commands the session wrote
// (.runsheet/validate.yml in its own checkout), so they run where the
// session runs — in its lease's pod — and never on the host of a node
// that sandboxes its sessions.
var ErrNoSuiteRunner = errors.New("run_suite cannot run here: the lease has no sandbox to run its suites in, and this node runs none on the host")

// RunSuite is one lease's run_suite tool.
type RunSuite struct {
	Workspace string
	Recorder  *Recorder
	// Run runs the steps: the lease's pod's runner on a sandboxed node.
	// Nil runs them on the host (HostSuites) only with OnHost, and
	// otherwise refuses (ErrNoSuiteRunner) before reading validate.yml.
	Run SuiteRunner
	// OnHost is the explicit opt-in to running the steps on the host
	// when Run is nil: only for a node whose sessions themselves run on
	// the host, unsandboxed (dev, test), where the steps can do nothing
	// the session could not. The zero value never runs on the host.
	OnHost bool
	// Escalate hears a red run, its summary why (PR 2.11, trigger
	// "red"), and answers the model the session moves to, "" when it
	// does not; nil never escalates.
	Escalate func(why string) string

	// A run outlives the call that starts it (ADR-049): the call waits
	// at most Wait (DefaultSuiteWait when zero) — under the MCP call's
	// own timeout — and then answers the run's handle, {run_id, status:
	// running}, which the session polls with run_suite {run_id}. The run
	// goes on in Context, the lease's (context.Background when nil), so
	// it ends with the lease and never with a call.
	Wait    time.Duration
	Context context.Context
	// Budget is the repository's validation budget (validate.yml's
	// budget:, read from the checkout as the lease began) and Began the
	// lease's start: a run still going at Began+Budget is stopped and
	// answered red, out of time, and a run asked for after it runs
	// nothing and is answered the same. Zero Budget bounds nothing.
	Budget time.Duration
	Began  time.Time

	mu   sync.Mutex
	runs map[string]*suiteRun
	n    int
}

// DefaultSuiteWait is how long one run_suite call waits on its run
// before it answers the run's handle: well under the 30 minutes a
// session's MCP call gives up after (mesh-specs gap 36).
const DefaultSuiteWait = 10 * time.Minute

// suiteRun is one run_suite run, started by a call and polled by its id.
type suiteRun struct {
	id      string
	suites  []string
	started time.Time
	done    chan struct{}
	answer  string
	err     error
}

// SuiteOutput is run_suite's output blob.
type SuiteOutput struct {
	Steps []SuiteStep  `json:"steps"`
	Tests []TestResult `json:"tests"`
}

// SuiteStep is one step's outcome; a sharded step's (validate.yml
// shards:) also each shard's, the step's exit the first failing
// shard's and its duration the longest shard's.
type SuiteStep struct {
	Name        string       `json:"name"`
	Command     string       `json:"command"`
	Exit        int          `json:"exit"`
	DurationMs  int64        `json:"duration_ms"`
	TestsRun    int          `json:"tests_run"`
	TestsPassed int          `json:"tests_passed"`
	TestsFailed int          `json:"tests_failed"`
	Shards      []SuiteShard `json:"shards,omitempty"`
}

// SuiteShard is one shard of a sharded step: its index (0-based, as
// MESH_SHARD_INDEX) of total, and how it ended — or that it never ran
// (not_run: a fail-fast run stopped first, or its lane did not open).
type SuiteShard struct {
	Index       int   `json:"index"`
	Total       int   `json:"total"`
	Exit        int   `json:"exit"`
	DurationMs  int64 `json:"duration_ms"`
	TestsRun    int   `json:"tests_run"`
	TestsPassed int   `json:"tests_passed"`
	TestsFailed int   `json:"tests_failed"`
	NotRun      bool  `json:"not_run,omitempty"`
}

// TestResult is one test's (or, Test empty, one package's) final event.
type TestResult struct {
	Package string  `json:"package"`
	Test    string  `json:"test"`
	Action  string  `json:"action"`
	Elapsed float64 `json:"elapsed"`
}

// Tool is run_suite {suite?, suites?, targets?, tests?}.
func (s *RunSuite) Tool() mcptool.Tool {
	list := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	return mcptool.Tool{
		Name: RunSuiteTool,
		Description: "Run the repository's declared validation (.runsheet/validate.yml) on your checkout as it stands, " +
			"every suite or the one you name, whole or only the targets and tests you name. A sharded suite's shards run at once, each beside its own services. " +
			"Answers each step's exit and test counts, the failing tests, " +
			"and the evidence id of the node's signed record of the run: pass that id on, never your own account of it. " +
			"A run of named targets or tests is recorded as that selection, never as the suite's full run. " +
			"A run still going after a while answers {run_id, status: running} instead: call run_suite {run_id} again to wait for it, as often as it takes; " +
			"never start the same suites again while it runs. A run past the repository's budget is stopped and answered red with out_of_time: finish red, saying it ran out of time.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"suite":  map[string]any{"type": "string", "description": "the suite to run, by name"},
				"suites": list("the suites to run, by name; every suite when neither suite nor suites is given"),
				"targets": list("the packages, paths or build targets to run, exported to the suite's command as " +
					SuiteTargetsEnv + " (space separated); the suite's whole set when absent"),
				"tests": list("the test names to run, exported to the suite's command as " + SuiteTestsEnv +
					" (space separated); every test when absent"),
				"fail_fast": map[string]any{"type": "boolean", "description": "stop the run at its first failing suite or shard, the others cancelled and named not run"},
				"run_id":    map[string]any{"type": "string", "description": "a running run's id, as an earlier call answered it: wait for that run instead of starting one"},
				"wait_seconds": map[string]any{"type": "integer", "minimum": 0,
					"description": "with run_id, how long to wait for the run before answering it is still running; at most the node's own wait, which is the default"},
			},
		},
		Call: s.call,
	}
}

// strings is a call's list argument, its blank entries dropped and the
// rest trimmed, in order.
func stringsArg(args map[string]any, key string) ([]string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is a list of strings", key)
	}
	var out []string
	for _, e := range raw {
		n, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("%s is a list of strings", key)
		}
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// selectionArg is the call's selection, nil when it named neither
// targets nor tests. A name holding whitespace could not be told apart
// in its space-separated variable, so it is refused.
func selectionArg(args map[string]any) (*Selection, error) {
	targets, err := stringsArg(args, "targets")
	if err != nil {
		return nil, err
	}
	tests, err := stringsArg(args, "tests")
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 && len(tests) == 0 {
		return nil, nil
	}
	for key, list := range map[string][]string{"targets": targets, "tests": tests} {
		if len(list) > maxSelection {
			return nil, fmt.Errorf("%s names at most %d", key, maxSelection)
		}
		for _, n := range list {
			if strings.ContainsFunc(n, unicode.IsSpace) {
				return nil, fmt.Errorf("%s entry %q holds whitespace: each is one name", key, n)
			}
		}
	}
	return &Selection{Targets: targets, Tests: tests}, nil
}

func (s *RunSuite) call(ctx context.Context, args map[string]any) (string, error) {
	if id, _ := args["run_id"].(string); strings.TrimSpace(id) != "" {
		wait, err := s.waitArg(args)
		if err != nil {
			return "", err
		}
		return s.poll(ctx, strings.TrimSpace(id), wait)
	}
	run := s.Run
	if run == nil {
		if !s.OnHost {
			return "", ErrNoSuiteRunner
		}
		run = HostSuites
	}
	names, err := stringsArg(args, "suites")
	if err != nil {
		return "", err
	}
	if one, _ := args["suite"].(string); strings.TrimSpace(one) != "" && !slices.Contains(names, strings.TrimSpace(one)) {
		names = append([]string{strings.TrimSpace(one)}, names...)
	}
	sel, err := selectionArg(args)
	if err != nil {
		return "", err
	}
	failFast := false
	if v, ok := args["fail_fast"]; ok && v != nil {
		if failFast, ok = v.(bool); !ok {
			return "", errors.New("fail_fast is true or false")
		}
	}
	cfg, err := validate.Load(s.Workspace)
	if errors.Is(err, validate.ErrNoValidation) {
		return "", errors.New("the repository declares no validation (.runsheet/validate.yml)")
	}
	if err != nil {
		return "", err
	}
	steps := cfg.Steps
	if len(names) > 0 {
		steps = nil
		for _, n := range names {
			i := slices.IndexFunc(cfg.Steps, func(st validate.Step) bool { return st.Name == n })
			if i < 0 {
				return "", fmt.Errorf("validate.yml has no suite %q", n)
			}
			steps = append(steps, cfg.Steps[i])
		}
	}
	if len(steps) == 0 {
		return "", errors.New("validate.yml declares no suites")
	}
	r := s.start(steps)
	go func() {
		defer close(r.done)
		r.answer, r.err = s.execute(r, run, steps, sel, failFast)
	}()
	return s.await(ctx, r, s.wait())
}

// wait is how long a call waits on its run.
func (s *RunSuite) wait() time.Duration {
	if s.Wait > 0 {
		return s.Wait
	}
	return DefaultSuiteWait
}

// waitArg is a poll's wait_seconds, at most the node's wait.
func (s *RunSuite) waitArg(args map[string]any) (time.Duration, error) {
	v, ok := args["wait_seconds"]
	if !ok || v == nil {
		return s.wait(), nil
	}
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case int:
		n = float64(x)
	default:
		n = -1
	}
	if n < 0 {
		return 0, errors.New("wait_seconds is a number of seconds, 0 or more")
	}
	return min(time.Duration(n*float64(time.Second)), s.wait()), nil
}

// start registers a run of steps under a new id.
func (s *RunSuite) start(steps []validate.Step) *suiteRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs == nil {
		s.runs = map[string]*suiteRun{}
	}
	s.n++
	r := &suiteRun{id: "run-" + strconv.Itoa(s.n), suites: stepNames(steps), started: time.Now(), done: make(chan struct{})}
	s.runs[r.id] = r
	return r
}

// poll waits up to wait for the run id names.
func (s *RunSuite) poll(ctx context.Context, id string, wait time.Duration) (string, error) {
	s.mu.Lock()
	r := s.runs[id]
	s.mu.Unlock()
	if r == nil {
		return "", fmt.Errorf("no run %q on this lease: run_suite answers a run's id when it starts one", id)
	}
	return s.await(ctx, r, wait)
}

// await is the run's answer once it has ended, or its handle when it
// is still going after wait. A call given up on by its caller leaves
// the run going.
func (s *RunSuite) await(ctx context.Context, r *suiteRun, wait time.Duration) (string, error) {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-r.done:
		return r.answer, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-t.C:
	}
	select {
	case <-r.done:
		return r.answer, r.err
	default:
	}
	handle := map[string]any{"run_id": r.id, "status": "running", "suites": r.suites,
		"started_at": r.started.UTC().Format(time.RFC3339), "elapsed_s": int(time.Since(r.started).Seconds()),
		"next": "the run goes on: call run_suite {\"run_id\": \"" + r.id + "\"} to wait for it again"}
	if d := s.deadline(); !d.IsZero() {
		handle["budget_left_s"] = max(int(time.Until(d).Seconds()), 0)
	}
	b, _ := json.Marshal(handle)
	return string(b), nil
}

// deadline is when the lease's budget runs out; zero for none.
func (s *RunSuite) deadline() time.Time {
	if s.Budget <= 0 || s.Began.IsZero() {
		return time.Time{}
	}
	return s.Began.Add(s.Budget)
}

// execute runs the steps to their end — or to the lease's budget — and
// records the run: its answer, as the call that sees it end returns it.
func (s *RunSuite) execute(r *suiteRun, run SuiteRunner, steps []validate.Step, sel *Selection, failFast bool) (string, error) {
	base := s.Context
	if base == nil {
		base = context.Background()
	}
	// reads of the checkout are not bounded by the budget
	readCtx := context.WithoutCancel(base)
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	deadline := s.deadline()
	if !deadline.IsZero() {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, deadline)
		defer stop()
	}
	if failFast {
		ctx = validate.WithFailFast(ctx)
	}
	head, _ := git(readCtx, s.Workspace, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	tree, _ := git(readCtx, s.Workspace, "rev-parse", "--verify", "-q", "HEAD^{tree}")
	// what the checkout holds beyond HEAD is part of what was run
	status, _ := git(readCtx, s.Workspace, "status", "--porcelain=v1", "-uall")
	start := time.Now()
	var results []validate.StepResult
	var runErr error
	if deadline.IsZero() || start.Before(deadline) {
		results, runErr = run(ctx, s.Workspace, steps, SelectionEnv(sel))
	}
	ended := time.Now()
	// out of time: the budget's deadline stopped a run that had not
	// passed whole, and not the lease's own end
	complete := len(results) == len(steps) && !slices.ContainsFunc(results, func(r validate.StepResult) bool { return r.ExitCode != 0 })
	outOfTime := !deadline.IsZero() && errors.Is(ctx.Err(), context.DeadlineExceeded) && base.Err() == nil && !complete
	if runErr != nil && len(results) == 0 && !outOfTime {
		return "", fmt.Errorf("the suites could not run: %v", runErr)
	}
	out := Normalise(results)
	blob, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	passed, failed, exit := 0, 0, 0
	var failing []string
	for _, st := range out.Steps {
		if st.Exit != 0 {
			exit = 1
		}
	}
	if len(results) < len(steps) || outOfTime {
		exit = 1
	}
	for _, t := range out.Tests {
		if t.Test == "" {
			continue
		}
		switch t.Action {
		case "pass":
			passed++
		case "fail":
			failed++
			if len(failing) < 20 {
				failing = append(failing, t.Package+"."+t.Test)
			}
		}
	}
	summary := fmt.Sprintf("%d of %d steps passed; %d tests passed, %d failed", countOK(out.Steps), len(steps), passed, failed)
	if sel != nil {
		summary += fmt.Sprintf(" (a selection: %d targets, %d tests)", len(sel.Targets), len(sel.Tests))
	}
	if outOfTime {
		summary = fmt.Sprintf("ran out of time: the lease ran past the repository's validation budget of %s (validate.yml budget:) and the run was stopped; ",
			s.Budget) + summary
	}
	input := map[string]any{"suites": stepNames(steps), "head": strings.TrimSpace(head), "tree": strings.TrimSpace(tree), "status": Digest([]byte(status))}
	if sel != nil {
		// two selections of one suite are two inputs
		input["selection"] = sel
	}
	rec := Record{Tool: RunSuiteTool, StartedAt: start, EndedAt: ended, Exit: exit, Summary: summary,
		InputDigest: InputDigest(input), Selection: sel}
	id, recErr := s.Recorder.Record(readCtx, rec, blob)
	answer := map[string]any{"run_id": r.id, "status": "finished", "exit": exit, "summary": summary, "steps": out.Steps, "failing_tests": failing,
		"head": strings.TrimSpace(head), "tree": strings.TrimSpace(tree), "uncommitted": strings.TrimSpace(status) != "", "output_digest": Digest(blob)}
	if sel != nil {
		answer["selection"] = sel
	}
	if outOfTime {
		answer["out_of_time"] = true
		answer["budget"] = s.Budget.String()
	} else if runErr != nil {
		answer["error"] = runErr.Error()
	}
	if recErr != nil {
		answer["evidence_error"] = recErr.Error()
	} else {
		answer["evidence_id"] = id
	}
	if exit != 0 && !outOfTime && s.Escalate != nil {
		if to := s.Escalate("run_suite went red: " + summary); to != "" {
			answer["escalated_to"] = to
		}
	}
	b, _ := json.Marshal(answer)
	return string(b), nil
}

// Normalise is the steps' outcomes and, from each step's go test -json
// report, every test's and package's final event (pass, fail or skip):
// run_suite's output blob.
func Normalise(results []validate.StepResult) SuiteOutput {
	out := SuiteOutput{Steps: []SuiteStep{}, Tests: []TestResult{}}
	for _, r := range results {
		out.Steps = append(out.Steps, SuiteStep{Name: r.Name, Command: r.Command, Exit: r.ExitCode, DurationMs: r.DurationMs,
			TestsRun: r.TestsRun, TestsPassed: r.TestsPassed, TestsFailed: r.TestsFailed, Shards: shardsOf(r.Shards)})
		out.Tests = append(out.Tests, goTestEvents(r.Report)...)
	}
	return out
}

// shardsOf are a sharded step's shards in index order, each one that
// never ran named not_run; nil for a step with no shards.
func shardsOf(shards []validate.ShardResult) []SuiteShard {
	if len(shards) == 0 || shards[0].Total <= 1 {
		return nil
	}
	total := shards[0].Total
	out := make([]SuiteShard, total)
	for k := range out {
		out[k] = SuiteShard{Index: k, Total: total, Exit: -1, NotRun: true}
	}
	for _, sh := range shards {
		if sh.Index < 0 || sh.Index >= total {
			continue
		}
		out[sh.Index] = SuiteShard{Index: sh.Index, Total: total, Exit: sh.ExitCode, DurationMs: sh.DurationMs,
			TestsRun: sh.TestsRun, TestsPassed: sh.TestsPassed, TestsFailed: sh.TestsFailed}
	}
	return out
}

// goTestEvents are a go test -json stream's final events, in order;
// any other line is passed over.
func goTestEvents(report string) []TestResult {
	var out []TestResult
	sc := bufio.NewScanner(strings.NewReader(report))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Action  string
			Package string
			Test    string
			Elapsed float64
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			out = append(out, TestResult{Package: ev.Package, Test: ev.Test, Action: ev.Action, Elapsed: ev.Elapsed})
		}
	}
	return out
}

func countOK(steps []SuiteStep) int {
	n := 0
	for _, s := range steps {
		if s.Exit == 0 {
			n++
		}
	}
	return n
}

func stepNames(steps []validate.Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}
