package node

import (
	"context"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// resultHarness ends every build with result, as given.
type resultHarness struct{ result harness.Result }

func (h *resultHarness) Name() string { return "claude-code" }
func (h *resultHarness) Run(_ context.Context, _ harness.Job, on func(harness.Progress)) (harness.Result, error) {
	if on != nil {
		on(harness.Progress{SessionID: "s-1"})
	}
	res := h.result
	res.SessionID = "s-1"
	return res, nil
}

const noFinishLine = "no mesh_finish call; parsed the final message"

// runFinish runs item1 to its end on result and returns the fake's
// finishWorkItem inputs, its failWorkItem count, what the node pushed
// and its log.
func runFinish(t *testing.T, result harness.Result, wrap func(Gateway) Gateway) ([]map[string]any, int, []string, string) {
	t.Helper()
	f := seed(t)
	r, ws, item, _ := runnerOnFake(t, f, &resultHarness{result: result}, wrap)
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatalf("RunItem: %v\n%s", err, logs)
	}
	var finishes []map[string]any
	for _, c := range f.CallsNamed("finishWorkItem") {
		finishes = append(finishes, c.Input)
	}
	return finishes, len(f.CallsNamed("failWorkItem")), ws.pushed, logs.String()
}

// A build that ends by calling mesh_finish, with no final message at
// all, is pushed and finished on the head the node pushed (ADR-023).
func TestBuildFinishesOnTheMeshFinishCall(t *testing.T) {
	finishes, fails, pushed, logs := runFinish(t, harness.Result{Success: true, Text: "", WorkCalls: map[string]map[string]any{
		"mesh_finish": {"item_id": item1, "branch": "mesh/g-1", "head_sha": "0000000"},
	}}, nil)
	if len(pushed) != 1 {
		t.Fatalf("pushed %v, want one push", pushed)
	}
	if len(finishes) != 1 || finishes[0]["headSha"] != "abc123def456" || finishes[0]["outcome"] != nil {
		t.Fatalf("finishWorkItem = %v, want one built finish on the pushed head", finishes)
	}
	if fails != 0 {
		t.Fatalf("failWorkItem called %d times", fails)
	}
	if strings.Contains(logs, noFinishLine) {
		t.Fatalf("a tool-call finish logged the fallback:\n%s", logs)
	}
}

// mesh_finish with outcome already_done hands in its evidence, no push.
func TestBuildFinishesAlreadyDoneOnTheMeshFinishCall(t *testing.T) {
	ev := "https://github.com/acme/widgets/pull/3 merged it"
	finishes, fails, pushed, logs := runFinish(t, harness.Result{Success: true, Text: "Done.", WorkCalls: map[string]map[string]any{
		"mesh_finish": {"item_id": item1, "outcome": "already_done", "evidence": ev},
	}}, nil)
	if len(pushed) != 0 {
		t.Fatalf("an already_done finish pushed %v", pushed)
	}
	if len(finishes) != 1 || finishes[0]["outcome"] != "already_done" || finishes[0]["evidence"] != ev {
		t.Fatalf("finishWorkItem = %v, want already_done with the evidence", finishes)
	}
	if fails != 0 || strings.Contains(logs, noFinishLine) {
		t.Fatalf("fails %d, logs:\n%s", fails, logs)
	}
}

// With no call the final message is parsed as before, and that is said
// once for the session.
func TestBuildWithNoFinishCallParsesTheFinalMessage(t *testing.T) {
	ev := "https://github.com/acme/widgets/pull/3 merged it"
	finishes, fails, pushed, logs := runFinish(t, harness.Result{Success: true, Text: "Checked main.\n\nALREADY DONE: " + ev, WorkCalls: map[string]map[string]any{}}, nil)
	if len(pushed) != 0 || fails != 0 {
		t.Fatalf("pushed %v, fails %d", pushed, fails)
	}
	if len(finishes) != 1 || finishes[0]["outcome"] != "already_done" || finishes[0]["evidence"] != ev {
		t.Fatalf("finishWorkItem = %v, want the parsed already_done", finishes)
	}
	if n := strings.Count(logs, noFinishLine); n != 1 {
		t.Fatalf("%q logged %d times, want 1:\n%s", noFinishLine, n, logs)
	}
}

// mesh_fail's error fails the item; nothing is pushed or finished.
func TestBuildFailsOnTheMeshFailCall(t *testing.T) {
	f := seed(t)
	r, ws, item, _ := runnerOnFake(t, f, &resultHarness{result: harness.Result{Success: true, WorkCalls: map[string]map[string]any{
		"mesh_fail": {"item_id": item1, "error": "the schema has no such field"},
	}}}, nil)
	r.Log = log.New(&syncBuf{}, "", 0)
	if err := r.RunItem(context.Background(), item); err == nil {
		t.Fatal("RunItem: want the failure")
	}
	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 || !strings.Contains(fails[0].Input["error"].(string), "the schema has no such field") {
		t.Fatalf("failWorkItem = %v", fails)
	}
	if len(ws.pushed) != 0 || len(f.CallsNamed("finishWorkItem")) != 0 {
		t.Fatalf("pushed %v, finished %d", ws.pushed, len(f.CallsNamed("finishWorkItem")))
	}
}

// stateRefusingGW refuses the node's own finish the way the platform
// does once the hosted mesh_finish moved the item to evaluating.
type stateRefusingGW struct{ Gateway }

func (stateRefusingGW) FinishWorkItem(context.Context, string, string, string, int, *gateway.UsageReport, ...string) error {
	return &gateway.Error{Message: "finishWorkItem: item is evaluating, not running"}
}

// The platform's hosted tool already handed the item in: the node's
// refused finish is no failure.
func TestBuildFinishedByToolCallOnThePlatform(t *testing.T) {
	_, fails, pushed, logs := runFinish(t, harness.Result{Success: true, WorkCalls: map[string]map[string]any{
		"mesh_finish": {"item_id": item1, "branch": "mesh/g-1", "head_sha": "abc123def456"},
	}}, func(g Gateway) Gateway { return stateRefusingGW{g} })
	if len(pushed) != 1 || fails != 0 {
		t.Fatalf("pushed %v, fails %d", pushed, fails)
	}
	if !strings.Contains(logs, "finished by tool call on the platform") {
		t.Fatalf("logs lack the tool-call finish:\n%s", logs)
	}
}

// A session's last mesh_progress note opening "for the docs writer:" is
// sent again after the node's own notes (its final text, the tool use)
// and before the finish, so it is the item's last note the docs writer
// folds in (ADR-025), never buried under the node's.
func TestBuildHandoffNoteIsTheLastNoteBeforeTheFinish(t *testing.T) {
	handoff := "for the docs writer: push, then attest, then finish"
	f := seed(t)
	r, _, item, _ := runnerOnFake(t, f, &resultHarness{result: harness.Result{Success: true, Text: "Built it.", WorkCalls: map[string]map[string]any{
		"mesh_progress": {"item_id": item1, "note": handoff},
		"mesh_finish":   {"item_id": item1, "branch": "mesh/g-1", "head_sha": "abc123def456"},
	}}}, nil)
	r.Log = log.New(&syncBuf{}, "", 0)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatalf("RunItem: %v", err)
	}
	last := ""
	for _, c := range f.Calls {
		switch c.Name {
		case "reportWorkItemProgress":
			last, _ = c.Input["note"].(string)
		case "finishWorkItem":
			if last != handoff {
				t.Fatalf("last note before finishWorkItem = %q, want %q", last, handoff)
			}
			return
		}
	}
	t.Fatal("no finishWorkItem call")
}

// A note not opening the prefix is not sent again.
func TestHandoffNoteNeedsThePrefix(t *testing.T) {
	if got := handoffNote(harness.Result{WorkCalls: map[string]map[string]any{"mesh_progress": {"note": "tests written"}}}); got != "" {
		t.Fatalf("handoffNote = %q, want none", got)
	}
	if got := handoffNote(harness.Result{}); got != "" {
		t.Fatalf("handoffNote of no call = %q", got)
	}
}
