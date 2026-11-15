package nodetools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

// scriptedPod is a lease's pod's runner: a suite's command writes the
// lines the script gives it and exits as it says; a command the
// supervisor told which tests to run (MESH_SUITE_TESTS exported before
// it) is a retry.
type scriptedPod struct {
	mu     sync.Mutex
	cmds   []string
	script func(cmd string, retry bool) (lines []string, exit int)
}

func (p *scriptedPod) Run(ctx context.Context, cmd string, _ time.Duration, stdout, _ io.Writer) (int, error) {
	p.mu.Lock()
	p.cmds = append(p.cmds, cmd)
	p.mu.Unlock()
	lines, exit := p.script(cmd, strings.Contains(cmd, "export "+SuiteTestsEnv+"="))
	for _, l := range lines {
		fmt.Fprintln(stdout, l)
	}
	if exit < 0 {
		// a command that never ends on its own
		<-ctx.Done()
		return -1, ctx.Err()
	}
	return exit, nil
}

func (p *scriptedPod) suites() SuiteRunner {
	return func(ctx context.Context, workspace string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		return validate.Run(ctx, steps, p, validate.Options{Dir: workspace, ContinueOnFailure: true})
	}
}

func goEvent(action, test string) string {
	b, _ := json.Marshal(map[string]string{"Action": action, "Package": "pkg", "Test": test})
	return string(b)
}

// What the node's supervisor found reaches the session's answer and the
// signed record's blob, through Normalise: a test that failed and
// passed when the node ran it again is flaky and its run green, one
// that failed again is a confirmed failure, a stalled test is named,
// and a suite that ran no tests is red with the reason.
func TestRunSuiteAnswersWhatTheSupervisorFound(t *testing.T) {
	failThenPass := func(test string) func(string, bool) ([]string, int) {
		return func(_ string, retry bool) ([]string, int) {
			if retry {
				return []string{goEvent("run", test), goEvent("pass", test)}, 0
			}
			return []string{goEvent("run", "TestOK"), goEvent("pass", "TestOK"), goEvent("run", test), goEvent("fail", test)}, 1
		}
	}
	for _, c := range []struct {
		name        string
		yml         string
		script      func(cmd string, retry bool) ([]string, int)
		wantExit    int
		wantOutcome map[string]string
		wantRetries map[string]int
		wantFlaky   []string
		wantFailing []string
		wantStalled []string
		wantSummary string
		wantStep    SuiteStep
		wantCmds    int
	}{
		{
			name:        "flaky: passes on retry, the run is green",
			yml:         "suites:\n  - name: unit\n    kind: unit\n    command: go test\n",
			script:      failThenPass("TestFlaky"),
			wantExit:    0,
			wantOutcome: map[string]string{"TestOK": "pass", "TestFlaky": "flaky"},
			wantRetries: map[string]int{"TestFlaky": 1},
			wantFlaky:   []string{"pkg.TestFlaky"},
			wantSummary: "1 of 1 steps passed; 1 tests passed, 0 failed, 0 skipped, 1 flaky (failed, then passed when the node ran them again)",
			wantStep:    SuiteStep{Name: "unit", Command: "go test", TestsRun: 2, TestsPassed: 1, TestsFlaky: 1},
			wantCmds:    2,
		},
		{
			name: "confirmed: fails again, the run is red and says how often it was retried",
			yml:  "suites:\n  - name: unit\n    kind: unit\n    command: go test\n",
			script: func(string, bool) ([]string, int) {
				return []string{goEvent("run", "TestBroken"), goEvent("fail", "TestBroken")}, 1
			},
			wantExit:    1,
			wantOutcome: map[string]string{"TestBroken": "fail"},
			wantRetries: map[string]int{"TestBroken": 1},
			wantFailing: []string{"pkg.TestBroken"},
			wantSummary: "0 of 1 steps passed; 0 tests passed, 1 failed, 0 skipped",
			wantStep:    SuiteStep{Name: "unit", Command: "go test", Exit: 1, TestsRun: 1, TestsFailed: 1},
			wantCmds:    2,
		},
		{
			name: "stalled: the silent test is named and its step stopped",
			yml:  "suites:\n  - name: unit\n    kind: unit\n    command: go test\n    stall: 30ms\n",
			script: func(string, bool) ([]string, int) {
				return []string{goEvent("run", "TestHangs")}, -1
			},
			wantExit:    1,
			wantOutcome: map[string]string{"TestHangs": "stalled"},
			wantStalled: []string{"pkg.TestHangs"},
			wantSummary: "0 skipped, 1 stalled",
			wantStep: SuiteStep{Name: "unit", Command: "go test", Exit: -1, Stopped: true,
				StopReason: "stalled: pkg.TestHangs produced no event for 30ms (validate.yml stall:)"},
			wantCmds: 1,
		},
		{
			name: "0 tests: a suite that exits 0 having run none is red",
			yml:  "suites:\n  - name: unit\n    kind: unit\n    command: go test\n",
			script: func(string, bool) ([]string, int) {
				return []string{`{"Action":"pass","Package":"pkg"}`}, 0
			},
			wantExit:    1,
			wantOutcome: map[string]string{},
			wantSummary: "0 of 1 steps passed; 0 tests passed, 0 failed, 0 skipped; unit ran 0 tests",
			wantStep:    SuiteStep{Name: "unit", Command: "go test", Exit: 1, Reason: "ran 0 tests"},
			wantCmds:    1,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := suiteCheckout(t, c.yml)
			m := &fakeMesh{}
			rec, _ := recorder(t, m, RunSuiteTool)
			pod := &scriptedPod{script: c.script}
			ans := answerOf(t)((&RunSuite{Workspace: dir, Recorder: rec, Run: pod.suites()}).Tool().Call(context.Background(), map[string]any{}))
			if ans["exit"] != float64(c.wantExit) {
				t.Fatalf("exit %v, want %d: %v", ans["exit"], c.wantExit, ans)
			}
			if summary, _ := ans["summary"].(string); !strings.Contains(summary, c.wantSummary) {
				t.Fatalf("summary %q, want %q", summary, c.wantSummary)
			}
			outcome, retries := map[string]string{}, map[string]int{}
			tests, _ := ans["tests"].([]any)
			for _, raw := range tests {
				e := raw.(map[string]any)
				outcome[e["name"].(string)] = e["outcome"].(string)
				if n, ok := e["retries"].(float64); ok {
					retries[e["name"].(string)] = int(n)
				}
			}
			if !reflect.DeepEqual(outcome, c.wantOutcome) {
				t.Fatalf("outcomes %v, want %v", outcome, c.wantOutcome)
			}
			if want := c.wantRetries; (len(want) > 0 || len(retries) > 0) && !reflect.DeepEqual(retries, want) {
				t.Fatalf("retries %v, want %v", retries, want)
			}
			for key, want := range map[string][]string{"flaky_tests": c.wantFlaky, "failing_tests": c.wantFailing, "stalled_tests": c.wantStalled} {
				var got []string
				if list, ok := ans[key].([]any); ok {
					for _, v := range list {
						got = append(got, v.(string))
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s %v, want %v", key, got, want)
				}
			}
			if n := len(pod.cmds); n != c.wantCmds {
				t.Fatalf("%d commands ran (%q), want %d", n, pod.cmds, c.wantCmds)
			}
			// the signed record's blob says the same
			st, _ := statementOf(t, m.records[len(m.records)-1])
			var out SuiteOutput
			if err := json.Unmarshal(m.blobs[st.Predicate.OutputDigest], &out); err != nil || len(out.Steps) != 1 {
				t.Fatalf("the blob is not the normalised run: %v %+v", err, out)
			}
			got := out.Steps[0]
			got.DurationMs = 0
			if !reflect.DeepEqual(got, c.wantStep) {
				t.Fatalf("the blob's step %+v, want %+v", got, c.wantStep)
			}
			blobOutcome := map[string]string{}
			for _, tr := range out.Tests {
				blobOutcome[tr.Test] = tr.Action
				if tr.Retries != c.wantRetries[tr.Test] {
					t.Fatalf("the blob's %s has retries %d, want %d", tr.Test, tr.Retries, c.wantRetries[tr.Test])
				}
			}
			if !reflect.DeepEqual(blobOutcome, c.wantOutcome) {
				t.Fatalf("the blob's outcomes %v, want %v", blobOutcome, c.wantOutcome)
			}
			if st.Predicate.Exit != c.wantExit {
				t.Fatalf("the record's exit %d, want %d", st.Predicate.Exit, c.wantExit)
			}
		})
	}
}

// Normalise carries a flaky test's action and retries, the step's flaky
// count and its reason: none of it is dropped on the way to the blob.
func TestNormaliseKeepsWhatTheSupervisorFound(t *testing.T) {
	got := Normalise([]validate.StepResult{{Name: "unit", Command: "go test", TestsRun: 2, TestsPassed: 1, TestsFlaky: 1, Reason: "r",
		Tests: []validate.TestResult{
			{Name: "TestFlaky", Package: "pkg", Action: validate.ActionFlaky, ElapsedMs: 20, Output: "boom\n", Retries: 1},
			{Name: "TestHangs", Package: "pkg", Action: validate.ActionStalled, ElapsedMs: 1},
		},
		Shards: []validate.ShardResult{
			{Index: 0, Total: 2, StepResult: validate.StepResult{TestsRun: 2, TestsFlaky: 1}},
			{Index: 1, Total: 2, StepResult: validate.StepResult{ExitCode: 1, Reason: "ran 0 tests"}},
		}}})
	want := SuiteOutput{
		Steps: []SuiteStep{{Name: "unit", Command: "go test", TestsRun: 2, TestsPassed: 1, TestsFlaky: 1, Reason: "r",
			Shards: []SuiteShard{{Index: 0, Total: 2, TestsRun: 2, TestsFlaky: 1}, {Index: 1, Total: 2, Exit: 1, Reason: "ran 0 tests"}}}},
		Tests: []TestResult{
			{Package: "pkg", Test: "TestFlaky", Action: "flaky", Elapsed: 0.02, Output: "boom\n", Retries: 1},
			{Package: "pkg", Test: "TestHangs", Action: "stalled", Elapsed: 0.001},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
