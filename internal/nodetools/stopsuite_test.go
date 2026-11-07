package nodetools

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

// A run_suite run still going is stopped one shard at a time by
// stop_suite, naming the run_id and the shard's step and index: that
// shard's eventual result, in the finished answer and the signed
// record, is stopped, non-zero and carries the given reason, the other
// shards' results are present and not stopped, and run_suite's own exit
// is never 0 once one shard was stopped.
func TestStopSuiteStopsOneShardLeavesTheOthersRunning(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n  - name: e2e\n    kind: integration\n    command: go test ./e2e\n    shards: 3\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)

	proceed := make(chan struct{}) // closed to let the un-stopped shards finish
	run := func(ctx context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		onLane := validate.OnLaneFunc(ctx)
		n := steps[0].Shards
		shards := make([]validate.ShardResult, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				lctx, cancel := context.WithCancel(ctx)
				defer cancel()
				var mu sync.Mutex
				var reason string
				if onLane != nil {
					onLane(fmt.Sprintf("%s-%d", steps[0].Name, i), func(r string) {
						mu.Lock()
						reason = r
						mu.Unlock()
						cancel()
					})
				}
				select {
				case <-lctx.Done():
					mu.Lock()
					rsn := reason
					mu.Unlock()
					shards[i] = validate.ShardResult{Index: i, Total: n,
						StepResult: validate.StepResult{ExitCode: 1, Stopped: true, StopReason: rsn}}
				case <-proceed:
					shards[i] = validate.ShardResult{Index: i, Total: n,
						StepResult: validate.StepResult{ExitCode: 0}}
				}
			}(i)
		}
		wg.Wait()
		// a sharded step's own result is its shards' merge (validate.merge):
		// a failing shard's exit, stop and reason are the step's too.
		top := validate.StepResult{Name: steps[0].Name, Shards: shards}
		for _, sh := range shards {
			if sh.ExitCode != 0 {
				top.ExitCode, top.Stopped, top.StopReason = sh.ExitCode, sh.Stopped, sh.StopReason
				break
			}
		}
		return []validate.StepResult{top}, nil
	}

	rs := &RunSuite{Workspace: dir, Recorder: rec, Run: run, Wait: 50 * time.Millisecond}
	runTool := rs.Tool()
	stopTool := (&StopSuite{Runs: rs}).Tool()

	first := answerOf(t)(runTool.Call(context.Background(), map[string]any{"suite": "e2e"}))
	id, _ := first["run_id"].(string)
	if first["status"] != "running" || id == "" {
		t.Fatalf("first answer %v, want a running handle", first)
	}

	stopAns := answerOf(t)(stopTool.Call(context.Background(), map[string]any{
		"run_id": id, "step": "e2e", "index": 1, "reason": "test: stopping shard 1",
	}))
	if stopAns["stopped"] != true || stopAns["index"] != float64(1) {
		t.Fatalf("stop_suite answer %v, want stopped index 1", stopAns)
	}
	close(proceed)

	var last map[string]any
	for polls := 0; polls < 50; polls++ {
		last = answerOf(t)(runTool.Call(context.Background(), map[string]any{"run_id": id}))
		if last["status"] != "running" {
			break
		}
	}
	if last["status"] != "finished" || last["exit"] != float64(1) || last["evidence_id"] == nil {
		t.Fatalf("finished answer %v, want exit 1 and recorded once a shard was stopped", last)
	}
	stepsOut := last["steps"].([]any)
	if len(stepsOut) != 1 {
		t.Fatalf("steps %v, want one", stepsOut)
	}
	step := stepsOut[0].(map[string]any)
	shards := step["shards"].([]any)
	if len(shards) != 3 {
		t.Fatalf("shards %v, want 3", shards)
	}
	for _, x := range shards {
		sh := x.(map[string]any)
		switch int(sh["index"].(float64)) {
		case 1:
			if sh["stopped"] != true || sh["exit"] == float64(0) || sh["stop_reason"] != "test: stopping shard 1" {
				t.Fatalf("shard 1 = %v, want stopped, non-zero exit, its reason", sh)
			}
		default:
			if sh["stopped"] == true || sh["exit"] != float64(0) {
				t.Fatalf("shard %v, want not stopped, exit 0", sh)
			}
		}
	}
	if len(m.records) != 1 {
		t.Fatalf("%d records, want the stopped run recorded once", len(m.records))
	}

	if _, err := stopTool.Call(context.Background(), map[string]any{
		"run_id": "run-99", "step": "e2e", "reason": "no such run",
	}); err == nil {
		t.Fatal("stop_suite on an unknown run_id was taken")
	}
	if _, err := stopTool.Call(context.Background(), map[string]any{
		"run_id": id, "step": "e2e", "index": 7, "reason": "no such shard",
	}); err == nil {
		t.Fatal("stop_suite on a lane that never opened was taken")
	}
}
