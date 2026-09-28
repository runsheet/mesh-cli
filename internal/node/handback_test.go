package node

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
)

// A build handed back with nothing committed (ADR 012 Hand-back) is
// pushed and finished like any other and takes the platform's
// empty-finish route back to ready; what it spent on is kept under the
// item, and the item's next claim names it in its prompt as "The last
// session on this item". A run to its end clears the record.
func TestHandedBackBuildGoesBackToTheBuilderAndBriefsTheNext(t *testing.T) {
	const spent = "It spent $2.61 of its $4.00 budget over 31 turns, at 812K input tokens a turn on average (1.4M at most), on tool results as large as Bash (220 KB) and calls to Read ×40, Bash ×22."
	var jobs []harness.Job
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		jobs = append(jobs, job)
		if len(jobs) == 1 {
			return harness.Result{Success: true, Text: "nothing worth committing yet", CostUSD: 2.9, SessionID: "s-1", HandedBack: true, SpentOn: spent}, nil
		}
		return harness.Result{Success: true, Text: "done", CostUSD: 1, SessionID: "s-2"}, nil
	})
	f := seed(t)
	f.EmptyHeads = map[string]bool{"abc123def456": true} // fakeWS's pushed head: nothing past the base
	r, ws, item, row := runnerOnFake(t, f, h, nil)
	r.State = &RunState{Dir: t.TempDir()}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if got := jobs[0]; got.HandBackAt != harness.DefaultHandBackAt || got.HasCommit == nil {
		t.Fatalf("the build's job has HandBackAt %v, HasCommit set %v", got.HandBackAt, got.HasCommit != nil)
	}
	if strings.Contains(jobs[0].Prompt, "The last session on this item") {
		t.Fatal("a first attempt's prompt names a last session")
	}
	if len(ws.pushed) != 1 {
		t.Fatalf("pushed %v: a handed-back build finishes as any other", ws.pushed)
	}
	if got := row(item1); got["status"] != "ready" || fmt.Sprint(got["attempts"]) != "1" {
		t.Fatalf("item after an empty hand-back = %v/%v, want ready, attempts 1", got["status"], got["attempts"])
	}
	if got := r.State.LastSession(item1); got != spent {
		t.Fatalf("recorded last session = %q", got)
	}

	// the item's next claim
	if err := r.GW.ClaimWorkItem(context.Background(), item1, r.Cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	item.Attempts = 1
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	next := jobs[1].Prompt
	if !strings.Contains(next, "## The last session on this item") || !strings.Contains(next, spent) || !strings.Contains(next, "read less, commit earlier") {
		t.Fatalf("the next attempt's prompt does not name the last session:\n%s", next)
	}
	if got := r.State.LastSession(item1); got != "" {
		t.Fatalf("a build run to its end left the record: %q", got)
	}
}
