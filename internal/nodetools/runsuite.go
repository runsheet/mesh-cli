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
// HostRunner in it).
type SuiteRunner func(ctx context.Context, workspace string, steps []validate.Step) ([]validate.StepResult, error)

// HostSuites runs steps on the host, in the workspace, every step run
// whatever an earlier one did.
func HostSuites(ctx context.Context, workspace string, steps []validate.Step) ([]validate.StepResult, error) {
	return validate.Run(ctx, steps, validate.HostRunner{Dir: workspace}, validate.Options{Dir: workspace, ContinueOnFailure: true})
}

// RunSuite is one lease's run_suite tool.
type RunSuite struct {
	Workspace string
	Recorder  *Recorder
	// Run runs the steps; HostSuites when nil.
	Run SuiteRunner
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

// Tool is run_suite {suites?}.
func (s *RunSuite) Tool() mcptool.Tool {
	return mcptool.Tool{
		Name: RunSuiteTool,
		Description: "Run the repository's declared validation (.runsheet/validate.yml) on your checkout as it stands, " +
			"every suite or the ones you name. Answers each step's exit and test counts, the failing tests, " +
			"and the evidence id of the node's signed record of the run: pass that id on, never your own account of it.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"suites": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "the suites to run, by name; every suite when absent"},
			},
		},
		Call: s.call,
	}
}

func (s *RunSuite) call(ctx context.Context, args map[string]any) (string, error) {
	var names []string
	if raw, ok := args["suites"].([]any); ok {
		for _, v := range raw {
			if n, ok := v.(string); ok && strings.TrimSpace(n) != "" {
				names = append(names, strings.TrimSpace(n))
			}
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
	head, _ := git(ctx, s.Workspace, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	tree, _ := git(ctx, s.Workspace, "rev-parse", "--verify", "-q", "HEAD^{tree}")
	// what the checkout holds beyond HEAD is part of what was run
	status, _ := git(ctx, s.Workspace, "status", "--porcelain=v1", "-uall")
	run := s.Run
	if run == nil {
		run = HostSuites
	}
	start := time.Now()
	results, runErr := run(ctx, s.Workspace, steps)
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
	rec := Record{Tool: RunSuiteTool, StartedAt: start, EndedAt: ended, Exit: exit, Summary: summary,
		InputDigest: InputDigest(map[string]any{"suites": stepNames(steps), "head": strings.TrimSpace(head), "tree": strings.TrimSpace(tree), "status": Digest([]byte(status))})}
	id, recErr := s.Recorder.Record(context.WithoutCancel(ctx), rec, blob)
	answer := map[string]any{"exit": exit, "summary": summary, "steps": out.Steps, "failing_tests": failing,
		"head": strings.TrimSpace(head), "tree": strings.TrimSpace(tree), "uncommitted": strings.TrimSpace(status) != "", "output_digest": Digest(blob)}
	if runErr != nil {
		answer["error"] = runErr.Error()
	}
	if recErr != nil {
		answer["evidence_error"] = recErr.Error()
	} else {
		answer["evidence_id"] = id
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
