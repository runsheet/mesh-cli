package nodetools

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

// laneRunner is a fake lane runner: each step's shards open a lane
// apiece (counted in opens, and announced on the ctx's OnLane), stream
// one passing test each, and hold until release is closed.
func laneRunner(opens *atomic.Int32, release <-chan struct{}) SuiteRunner {
	return func(ctx context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		onLane, onTest := validate.OnLaneFunc(ctx), validate.OnTestFunc(ctx)
		var out []validate.StepResult
		for _, st := range steps {
			n := max(st.Shards, 1)
			for i := range n {
				opens.Add(1)
				if onLane != nil {
					onLane(fmt.Sprintf("%s-%d", st.Name, i), func(string) {})
				}
				if onTest != nil {
					onTest(validate.TestResult{Name: fmt.Sprintf("Test%s%d", st.Name, i), Package: "pkg", Action: "pass"})
				}
			}
			out = append(out, validate.StepResult{Name: st.Name, Command: st.Command})
		}
		select {
		case <-release:
		case <-ctx.Done():
			return out, ctx.Err()
		}
		return out, nil
	}
}

// waitLanes waits until n lanes have opened.
func waitLanes(t *testing.T, opens *atomic.Int32, n int32) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); opens.Load() < n; {
		if time.Now().After(end) {
			t.Fatalf("%d lanes opened, want %d", opens.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A second run_suite call for a suite whose lanes are still running —
// naming no run_id, as the session on 2026-11-11 did again and again —
// opens no lane: it answers the running run, status running, with the
// tests so far and the same run_id, and the run is recorded once.
func TestRunSuiteRepeatCallAnswersTheRunningRun(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: e2e\n    kind: integration\n    command: go test ./e2e\n    shards: 3\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	var opens atomic.Int32
	release := make(chan struct{})
	tool := (&RunSuite{Workspace: dir, Recorder: rec, Run: laneRunner(&opens, release), Wait: 50 * time.Millisecond}).Tool()

	first := answerOf(t)(tool.Call(context.Background(), map[string]any{"suite": "e2e"}))
	id, _ := first["run_id"].(string)
	if first["status"] != "running" || id == "" {
		t.Fatalf("first answer %v, want a running handle", first)
	}
	waitLanes(t, &opens, 3)
	for _, args := range []map[string]any{{"suite": "e2e"}, {"suites": []any{"e2e"}}, {}} {
		again := answerOf(t)(tool.Call(context.Background(), args))
		if again["status"] != "running" || again["run_id"] != id {
			t.Fatalf("repeat call %v answered %v, want run %s still running", args, again, id)
		}
		if tests, _ := again["tests"].([]any); len(tests) != 3 {
			t.Fatalf("repeat call's tests %v, want the 3 so far", again["tests"])
		}
		if again["note"] != nil {
			t.Fatalf("the same selection carried a note: %v", again["note"])
		}
	}
	if opens.Load() != 3 {
		t.Fatalf("%d lanes opened, want the first run's 3 alone", opens.Load())
	}
	// a narrower selection of the running suite starts nothing either
	narrow := answerOf(t)(tool.Call(context.Background(), map[string]any{"suite": "e2e", "tests": []any{"TestOne"}}))
	if narrow["status"] != "running" || narrow["run_id"] != id || !strings.Contains(fmt.Sprint(narrow["note"]), "started nothing") || opens.Load() != 3 {
		t.Fatalf("an overlapping selection answered %v with %d lanes, want run %s and a note", narrow, opens.Load(), id)
	}

	close(release)
	var last map[string]any
	for polls := 0; polls < 50; polls++ {
		last = answerOf(t)(tool.Call(context.Background(), map[string]any{"run_id": id}))
		if last["status"] != "running" {
			break
		}
	}
	if last["status"] != "finished" || len(m.records) != 1 || opens.Load() != 3 {
		t.Fatalf("final %v, %d records, %d lanes", last, len(m.records), opens.Load())
	}
	// once its end has been answered, the same call runs the suite again
	rerun := answerOf(t)(tool.Call(context.Background(), map[string]any{"suite": "e2e"}))
	waitLanes(t, &opens, 6)
	if rerun["run_id"] == id || opens.Load() != 6 {
		t.Fatalf("a call after the run ended answered %v with %d lanes, want a fresh run", rerun, opens.Load())
	}
}

// A call whose suites overlap no running run starts its own beside it.
func TestRunSuiteDifferentSuiteStillStarts(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: unit\n    kind: unit\n    command: go test ./...\n  - name: e2e\n    kind: integration\n    command: go test ./e2e\n    shards: 2\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	var opens atomic.Int32
	release := make(chan struct{})
	defer close(release)
	tool := (&RunSuite{Workspace: dir, Recorder: rec, Run: laneRunner(&opens, release), Wait: 50 * time.Millisecond}).Tool()

	e2e := answerOf(t)(tool.Call(context.Background(), map[string]any{"suite": "e2e"}))
	unit := answerOf(t)(tool.Call(context.Background(), map[string]any{"suite": "unit"}))
	waitLanes(t, &opens, 3)
	if e2e["status"] != "running" || unit["status"] != "running" || e2e["run_id"] == unit["run_id"] || opens.Load() != 3 {
		t.Fatalf("e2e %v, unit %v, %d lanes: want two runs, 3 lanes", e2e, unit, opens.Load())
	}
	// every suite overlaps both: it answers the oldest, naming the other
	all := answerOf(t)(tool.Call(context.Background(), map[string]any{}))
	if all["run_id"] != e2e["run_id"] || !strings.Contains(fmt.Sprint(all["note"]), fmt.Sprint(unit["run_id"])) || opens.Load() != 3 {
		t.Fatalf("every suite answered %v with %d lanes, want %v noting %v", all, opens.Load(), e2e["run_id"], unit["run_id"])
	}
}

// A run that ended while no call was waiting on it — its caller gave up
// — answers its result to the next call for the same selection rather
// than running again.
func TestRunSuiteUnansweredEndAnswersTheRepeatCall(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: unit\n    kind: unit\n    command: go test ./...\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	var opens atomic.Int32
	release := make(chan struct{})
	rs := &RunSuite{Workspace: dir, Recorder: rec, Run: laneRunner(&opens, release), Wait: 50 * time.Millisecond}
	tool := rs.Tool()
	first := answerOf(t)(tool.Call(context.Background(), map[string]any{"suite": "unit"}))
	close(release)
	<-rs.find(first["run_id"].(string)).done
	again := answerOf(t)(tool.Call(context.Background(), map[string]any{"suite": "unit"}))
	if again["run_id"] != first["run_id"] || again["status"] != "finished" || opens.Load() != 1 {
		t.Fatalf("answered %v with %d lanes, want run %v's result", again, opens.Load(), first["run_id"])
	}
}
