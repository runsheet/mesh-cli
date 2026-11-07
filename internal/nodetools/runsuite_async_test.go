package nodetools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

// suiteCheckout is a git checkout whose validate.yml is yml.
func suiteCheckout(t *testing.T, yml string) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	if err := os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// answerOf is a run_suite call's answer as JSON.
func answerOf(t *testing.T) func(string, error) map[string]any {
	return func(s string, err error) map[string]any { return decodeAnswer(t, s, err) }
}

func decodeAnswer(t *testing.T, s string, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("answer %q: %v", s, err)
	}
	return m
}

// A run that outlasts the call's wait — the MCP call's timeout, here a
// short one — answers its handle, never a timeout: the run goes on, a
// call naming its run_id waits for it again, and the call that sees it
// end answers the result with its evidence id. The run is recorded once.
func TestRunSuiteAnswersAHandleTheSessionPolls(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: integration\n    kind: integration\n    command: go test ./e2e\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	var runs atomic.Int32
	run := func(ctx context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		runs.Add(1)
		select {
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []validate.StepResult{{Name: steps[0].Name, Command: steps[0].Command}}, nil
	}
	tool := (&RunSuite{Workspace: dir, Recorder: rec, Run: run, Wait: 50 * time.Millisecond}).Tool()
	// the call's own ctx ends with it, as a session's MCP call that gave
	// up does: the run is not stopped by it
	callCtx, cancel := context.WithCancel(context.Background())
	first := answerOf(t)(tool.Call(callCtx, map[string]any{"suite": "integration"}))
	cancel()
	id, _ := first["run_id"].(string)
	if first["status"] != "running" || id == "" || first["evidence_id"] != nil {
		t.Fatalf("first answer %v, want a running handle", first)
	}
	var last map[string]any
	for polls := 0; polls < 50; polls++ {
		last = answerOf(t)(tool.Call(context.Background(), map[string]any{"run_id": id}))
		if last["status"] != "running" {
			break
		}
	}
	if last["status"] != "finished" || last["run_id"] != id || last["exit"] != float64(0) || last["evidence_id"] == nil {
		t.Fatalf("polled answer %v, want the finished run with its evidence", last)
	}
	again := answerOf(t)(tool.Call(context.Background(), map[string]any{"run_id": id, "wait_seconds": 0}))
	if again["evidence_id"] != last["evidence_id"] {
		t.Fatalf("a finished run answered %v again, want the same", again)
	}
	if runs.Load() != 1 || len(m.records) != 1 {
		t.Fatalf("%d runs and %d records, want one of each", runs.Load(), len(m.records))
	}
	if _, err := tool.Call(context.Background(), map[string]any{"run_id": "run-99"}); err == nil || !strings.Contains(err.Error(), "no run") {
		t.Fatalf("an unknown run_id answered %v", err)
	}
}

// A validation is held to its repository's budget: a run still going
// when the lease's budget runs out is stopped and answered red, out of
// time, with a signed record saying so; a run asked for after it runs
// nothing and is answered the same.
func TestRunSuitePastItsBudgetEndsRed(t *testing.T) {
	dir := suiteCheckout(t, "budget: 80m\nsuites:\n  - name: integration\n    kind: integration\n    command: go test ./e2e\n    shards: 4\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	var runs atomic.Int32
	run := func(ctx context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		runs.Add(1)
		<-ctx.Done() // a shard that would run on past the budget
		return []validate.StepResult{{Name: steps[0].Name, Command: steps[0].Command, ExitCode: -1}}, ctx.Err()
	}
	rs := &RunSuite{Workspace: dir, Recorder: rec, Run: run, Wait: time.Minute, Budget: 300 * time.Millisecond, Began: time.Now()}
	tool := rs.Tool()
	ans := answerOf(t)(tool.Call(context.Background(), map[string]any{"suite": "integration"}))
	if ans["exit"] != float64(1) || ans["out_of_time"] != true || !strings.Contains(ans["summary"].(string), "ran out of time") || ans["evidence_id"] == nil {
		t.Fatalf("answer %v, want red, out of time, recorded", ans)
	}
	if len(m.records) != 1 {
		t.Fatalf("%d records", len(m.records))
	}
	if st, _ := statementOf(t, m.records[0]); st.Predicate.Exit != 1 || !strings.Contains(st.Predicate.Summary, "ran out of time") || !strings.Contains(st.Predicate.Summary, "300ms") {
		t.Fatalf("the record says exit %d %q", st.Predicate.Exit, st.Predicate.Summary)
	}
	late := answerOf(t)(tool.Call(context.Background(), map[string]any{"suite": "integration"}))
	if late["exit"] != float64(1) || late["out_of_time"] != true || runs.Load() != 1 {
		t.Fatalf("a run after the budget answered %v and ran %d times", late, runs.Load())
	}
}

// A run that passes whole before its budget is green, however close.
func TestRunSuiteInsideItsBudgetIsGreen(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: unit\n    kind: unit\n    command: go test ./...\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	run := func(_ context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		return []validate.StepResult{{Name: steps[0].Name}}, nil
	}
	ans := answerOf(t)((&RunSuite{Workspace: dir, Recorder: rec, Run: run, Budget: time.Hour, Began: time.Now()}).Tool().Call(context.Background(), map[string]any{}))
	if ans["exit"] != float64(0) || ans["out_of_time"] != nil {
		t.Fatalf("answer %v", ans)
	}
}

// fail_fast reaches the runner on its ctx (validate.WithFailFast), and a
// sharded step's shards are each in the output, one that never ran
// named not_run.
func TestRunSuiteFailFastAndShards(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: integration\n    kind: integration\n    command: go test ./e2e\n    shards: 3\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	var asked bool
	run := func(ctx context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		asked = validate.FailFastAsked(ctx)
		sh := []validate.ShardResult{
			{Index: 0, Total: 3, StepResult: validate.StepResult{ExitCode: 0, DurationMs: 10, TestsRun: 2, TestsPassed: 2}},
			{Index: 1, Total: 3, StepResult: validate.StepResult{ExitCode: 1, DurationMs: 5, TestsRun: 1, TestsFailed: 1}},
		}
		return []validate.StepResult{{Name: steps[0].Name, ExitCode: 1, DurationMs: 10, Shards: sh}}, nil
	}
	ans := answerOf(t)((&RunSuite{Workspace: dir, Recorder: rec, Run: run}).Tool().Call(context.Background(), map[string]any{"fail_fast": true}))
	if !asked {
		t.Fatal("fail_fast did not reach the runner")
	}
	steps := ans["steps"].([]any)
	shards := steps[0].(map[string]any)["shards"].([]any)
	if len(shards) != 3 || shards[1].(map[string]any)["exit"] != float64(1) || shards[2].(map[string]any)["not_run"] != true {
		t.Fatalf("shards %v", shards)
	}
	var out SuiteOutput
	if err := json.Unmarshal(m.blobs[ans["output_digest"].(string)], &out); err != nil || len(out.Steps[0].Shards) != 3 {
		t.Fatalf("the signed output's shards: %+v %v", out, err)
	}
	if _, err := (&RunSuite{Workspace: dir, Recorder: rec, Run: run}).Tool().Call(context.Background(), map[string]any{"fail_fast": "yes"}); err == nil {
		t.Fatal("a fail_fast that is not a boolean was taken")
	}
}

// A poll mid-run answers each test's result as it streams in
// (validate.WithOnTest, which the runner's ctx carries to Options.OnTest):
// a session watching a long suite sees a failure as it happens, not only
// at the end. The finished answer's tests are Normalise's own list, never
// the live buffer, so a poll after the run ends does not repeat it.
func TestRunSuitePollSeesEachTestAsItHappens(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: unit\n    kind: unit\n    command: go test ./...\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)

	tick := make(chan struct{})
	proceed := make(chan struct{})
	run := func(ctx context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		onTest := validate.OnTestFunc(ctx)
		tests := make([]validate.TestResult, 10)
		for i := 1; i <= 10; i++ {
			name := fmt.Sprintf("Test%d", i)
			tr := validate.TestResult{Name: name, Package: "pkg", Action: "pass", ElapsedMs: 10}
			if i == 3 {
				tr.Action = "fail"
				tr.Output = "it broke\n"
			}
			tests[i-1] = tr
			if onTest != nil {
				onTest(tr)
			}
			tick <- struct{}{}
			<-proceed
		}
		return []validate.StepResult{{Name: steps[0].Name, ExitCode: 1, TestsRun: 10, TestsFailed: 1, Tests: tests}}, nil
	}
	tool := (&RunSuite{Workspace: dir, Recorder: rec, Run: run, Wait: 20 * time.Millisecond}).Tool()

	type callResult struct {
		s   string
		err error
	}
	firstCh := make(chan callResult, 1)
	go func() {
		s, err := tool.Call(context.Background(), map[string]any{})
		firstCh <- callResult{s, err}
	}()

	for i := 1; i <= 2; i++ {
		<-tick
		proceed <- struct{}{}
	}
	<-tick // Test3's failure has streamed; held here before Test4 runs

	cr := <-firstCh
	first := decodeAnswer(t, cr.s, cr.err)
	id, _ := first["run_id"].(string)
	if first["status"] != "running" || id == "" {
		t.Fatalf("first answer %v, want a running handle", first)
	}

	mid := answerOf(t)(tool.Call(context.Background(), map[string]any{"run_id": id, "wait_seconds": 0}))
	if mid["status"] != "running" {
		t.Fatalf("mid-run poll %v, want still running", mid)
	}
	midTests, _ := mid["tests"].([]any)
	var sawThird bool
	for _, x := range midTests {
		e := x.(map[string]any)
		if e["name"] == "Test10" {
			t.Fatalf("mid-run poll already carries Test10, strictly before its event: %v", midTests)
		}
		if e["name"] == "Test3" {
			sawThird = true
			if e["outcome"] != "fail" || e["output"] != "it broke\n" {
				t.Fatalf("Test3 in the mid-run poll = %v, want its failure and output", e)
			}
		}
	}
	if !sawThird {
		t.Fatalf("mid-run poll %v, want Test3's failure", midTests)
	}

	proceed <- struct{}{} // let Test3's iteration finish
	for i := 4; i <= 10; i++ {
		<-tick
		proceed <- struct{}{}
	}

	var last map[string]any
	for polls := 0; polls < 50; polls++ {
		last = answerOf(t)(tool.Call(context.Background(), map[string]any{"run_id": id}))
		if last["status"] != "running" {
			break
		}
	}
	if last["status"] != "finished" {
		t.Fatalf("final poll %v, want finished", last)
	}
	lastTests, _ := last["tests"].([]any)
	if len(lastTests) != 10 {
		t.Fatalf("finished answer's tests %v, want all 10 of Normalise's own list", lastTests)
	}
	var thirdCount int
	for _, x := range lastTests {
		if x.(map[string]any)["name"] == "Test3" {
			thirdCount++
		}
	}
	if thirdCount != 1 {
		t.Fatalf("Test3 appears %d times in the finished answer, want exactly once: not repeated from the mid-run poll", thirdCount)
	}

	again := answerOf(t)(tool.Call(context.Background(), map[string]any{"run_id": id, "wait_seconds": 0}))
	if again["evidence_id"] != last["evidence_id"] {
		t.Fatalf("a finished run answered %v again, want the same", again)
	}
}
