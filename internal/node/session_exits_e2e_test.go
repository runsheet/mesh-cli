//go:build e2e

package node

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// A build session that exits at once — unsuccessful, "done" with no
// mesh_finish and nothing committed, a harness error, its runsheet server
// failed to start (the dl360 incident's shape, a-red-main-is-not-the-
// items-fault/43), or the runtime unable to start its pod — reaches the
// platform within a minute of its end: failWorkItem with the session's
// text or tools note, or releaseWorkItem for the infrastructure cause,
// and the item is never left claimed or running for the stall sweep.
func TestSessionThatExitsAtOnceIsReportedE2E(t *testing.T) {
	runsheetFailed := map[string]*harness.ToolStat{"runsheet": {Errors: 1, FirstError: "failed"}}
	cases := []struct {
		name     string
		result   harness.Result
		err      error
		mutation string
		want     string
		status   string
	}{
		{name: "unsuccessful", result: harness.Result{Success: false, Text: "error_max_turns: I ran out of turns"},
			mutation: "failWorkItem", want: "I ran out of turns", status: "failed"},
		{name: "done with no finish", result: harness.Result{Success: true, Text: "All done, the tests pass."},
			mutation: "failWorkItem", want: "All done, the tests pass.", status: "failed"},
		{name: "harness error", err: errors.New("claude exited 1: segmentation fault"),
			mutation: "failWorkItem", want: "segmentation fault", status: "failed"},
		{name: "runsheet server failed", result: harness.Result{Success: true, Text: "mesh_finish is not available; finishing here.", Tools: runsheetFailed, ToolCalls: map[string]int{}},
			mutation: "failWorkItem", want: "runsheet served, never called: failed", status: "failed"},
		{name: "runtime cannot start the pod", err: &sandbox.RuntimeError{Op: "pod create", Err: errors.New("error opening storage")},
			mutation: "releaseWorkItem", want: "error opening storage", status: "ready"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var ended time.Time
			h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
				time.Sleep(time.Second)
				mu.Lock()
				ended = time.Now()
				mu.Unlock()
				return c.result, c.err
			})
			f := seed(t)
			r, _, item, row := runnerOnFake(t, f, h, nil)
			r.HasCommit = func(string, string) bool { return false }
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			r.RunItem(ctx, item)

			calls := f.CallsNamed(c.mutation)
			if len(calls) == 0 {
				t.Fatalf("no %s; calls: %v", c.mutation, f.Calls)
			}
			key := "error"
			if c.mutation == "releaseWorkItem" {
				key = "note"
			}
			reason, _ := calls[0].Input[key].(string)
			if !strings.Contains(reason, c.want) {
				t.Fatalf("%s %s = %q, want it to name %q", c.mutation, key, reason, c.want)
			}
			mu.Lock()
			took := calls[0].At.Sub(ended)
			mu.Unlock()
			if took > time.Minute {
				t.Fatalf("%s came %s after the session's end", c.mutation, took)
			}
			if c.mutation == "failWorkItem" && len(f.CallsNamed("finishWorkItem")) > 0 {
				t.Fatalf("an unfinished session was finished: %v", f.CallsNamed("finishWorkItem"))
			}
			if st := row(item1)["status"]; st != c.status {
				t.Fatalf("item left %v, want %s", st, c.status)
			}
		})
	}
}
