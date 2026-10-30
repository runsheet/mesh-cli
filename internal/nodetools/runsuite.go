package nodetools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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
}

// SuiteOutput is run_suite's output blob.
type SuiteOutput struct {
	Steps []SuiteStep  `json:"steps"`
	Tests []TestResult `json:"tests"`
}

// SuiteStep is one step's outcome.
type SuiteStep struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Exit        int    `json:"exit"`
	DurationMs  int64  `json:"duration_ms"`
	TestsRun    int    `json:"tests_run"`
	TestsPassed int    `json:"tests_passed"`
	TestsFailed int    `json:"tests_failed"`
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
			"every suite or the one you name, whole or only the targets and tests you name. Answers each step's exit and test counts, the failing tests, " +
			"and the evidence id of the node's signed record of the run: pass that id on, never your own account of it. " +
			"A run of named targets or tests is recorded as that selection, never as the suite's full run.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"suite":  map[string]any{"type": "string", "description": "the suite to run, by name"},
				"suites": list("the suites to run, by name; every suite when neither suite nor suites is given"),
				"targets": list("the packages, paths or build targets to run, exported to the suite's command as " +
					SuiteTargetsEnv + " (space separated); the suite's whole set when absent"),
				"tests": list("the test names to run, exported to the suite's command as " + SuiteTestsEnv +
					" (space separated); every test when absent"),
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
	head, _ := git(ctx, s.Workspace, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	tree, _ := git(ctx, s.Workspace, "rev-parse", "--verify", "-q", "HEAD^{tree}")
	// what the checkout holds beyond HEAD is part of what was run
	status, _ := git(ctx, s.Workspace, "status", "--porcelain=v1", "-uall")
	start := time.Now()
	results, runErr := run(ctx, s.Workspace, steps, SelectionEnv(sel))
	ended := time.Now()
	if runErr != nil && len(results) == 0 {
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
	if len(results) < len(steps) {
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
	input := map[string]any{"suites": stepNames(steps), "head": strings.TrimSpace(head), "tree": strings.TrimSpace(tree), "status": Digest([]byte(status))}
	if sel != nil {
		// two selections of one suite are two inputs
		input["selection"] = sel
	}
	rec := Record{Tool: RunSuiteTool, StartedAt: start, EndedAt: ended, Exit: exit, Summary: summary,
		InputDigest: InputDigest(input), Selection: sel}
	id, recErr := s.Recorder.Record(context.WithoutCancel(ctx), rec, blob)
	answer := map[string]any{"exit": exit, "summary": summary, "steps": out.Steps, "failing_tests": failing,
		"head": strings.TrimSpace(head), "tree": strings.TrimSpace(tree), "uncommitted": strings.TrimSpace(status) != "", "output_digest": Digest(blob)}
	if sel != nil {
		answer["selection"] = sel
	}
	if runErr != nil {
		answer["error"] = runErr.Error()
	}
	if recErr != nil {
		answer["evidence_error"] = recErr.Error()
	} else {
		answer["evidence_id"] = id
	}
	if exit != 0 && s.Escalate != nil {
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
			TestsRun: r.TestsRun, TestsPassed: r.TestsPassed, TestsFailed: r.TestsFailed})
		out.Tests = append(out.Tests, goTestEvents(r.Report)...)
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
