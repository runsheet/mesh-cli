package node

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// nodeVerdicts is how many evaluate finishes the node sent with a
// verdict of its own ({verdict}) rather than closing with {result}.
func nodeVerdicts(f *gatewaytest.Fake) int {
	n := 0
	for _, c := range f.CallsNamed("finishJob") {
		if fin, _ := c.Input["finish"].(map[string]any); c.Input["kind"] == EvaluateKind && fin["verdict"] != nil {
			n++
		}
	}
	return n
}

// The evaluator session hands its verdict to the product's own tool:
// the brief names mesh_verdict, the session is served the product's
// server, and the verdict the item moves on is the one that tool took —
// the node judges nothing itself (runsheet ADR 077).
func TestTheVerdictReachesTheProductThroughTheSessionsTool(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "VERDICT: FAIL")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	ev.MCP = MeshContextHTTP(f.URL(), "rs_test")
	verdict := map[string]any{"item_id": item1, "passed": true, "notes": "The test fails as asked."}
	claude.workCalls = map[string]map[string]any{"mesh_verdict": verdict}
	f.HandedInVerdicts = map[string]map[string]any{item1: {"passed": true, "notes": "The test fails as asked."}}
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
	if len(claude.jobs) != 1 || len(codex.jobs) != 0 {
		t.Fatalf("claude ran %d, codex %d", len(claude.jobs), len(codex.jobs))
	}
	if !strings.Contains(claude.jobs[0].Prompt, "mesh_verdict") || claude.jobs[0].MCPServers["runsheet"] == nil {
		t.Fatalf("the session was not handed the product's tool: %+v", claude.jobs[0].MCPServers)
	}
	// the session's own text says FAIL; the product's tool said pass
	if f.Item(item1)["status"] != "review" {
		t.Fatalf("item = %v, want review on the handed-in pass", f.Item(item1)["status"])
	}
	if n := nodeVerdicts(f); n != 0 {
		t.Fatalf("the node sent %d verdicts of its own", n)
	}
}

// The node closes the job with its notes tail alone — who judged, and
// the session's cost and usage — and nothing that reads as a verdict.
func TestTheNodeClosesWithItsNotesTail(t *testing.T) {
	f, ev, claude, _, _, _ := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	f.HandedInVerdicts = map[string]map[string]any{item1: {"passed": true, "notes": "Holds."}}
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
	if len(claude.jobs) != 1 {
		t.Fatalf("claude ran %d", len(claude.jobs))
	}
	closed := evaluationClosed(t, f)
	fin, _ := closed["finish"].(map[string]any)
	if len(fin) != 1 || fin["result"] == nil {
		t.Fatalf("finish = %v, want {result} alone", fin)
	}
	res, _ := fin["result"].(map[string]any)
	for k := range res {
		if k != "notes_tail" && k != "cost_cents" && k != "usage" {
			t.Fatalf("result carries %q: %v", k, res)
		}
	}
	if res["notes_tail"] != "(evaluated on Claude sonnet)" {
		t.Fatalf("notes_tail = %q", res["notes_tail"])
	}
	if nodeVerdicts(f) != 0 || evaluateJobsClosed(f) != 1 {
		t.Fatalf("calls: %+v", f.Calls)
	}
}

// A session that could see nothing to judge hands the product a
// no_verdict with its reason: the item stays in evaluating with no
// attempt spent, the node still closes the job once, and no blind retry
// runs on the other family — the product decides what comes next.
func TestABlindSessionsNoVerdictGoesToTheProduct(t *testing.T) {
	reason := "the runsheet tools answered nothing; no context to judge on"
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "I could see nothing.", "I could see nothing.")
	claude.workCalls = map[string]map[string]any{"mesh_verdict": {"item_id": item1, "no_verdict": true, "no_verdict_reason": reason}}
	codex.text = "I could see nothing."
	f.HandedInVerdicts = map[string]map[string]any{item1: {"passed": false, "noVerdict": true, "noVerdictReason": reason}}
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatalf("Evaluate: %v\n%s", err, logs.String())
	}
	if n := len(claude.jobs) + len(codex.jobs); n != 1 {
		t.Fatalf("sessions = %d (claude %d, codex %d), want one: no blind retry", n, len(claude.jobs), len(codex.jobs))
	}
	row := f.Item(item1)
	if row["status"] != "evaluating" || row["noVerdictReason"] != reason || row["noVerdicts"] != 1 {
		t.Fatalf("row = %v", row)
	}
	if nodeVerdicts(f) != 0 || evaluateJobsClosed(f) != 1 {
		t.Fatalf("calls: %+v", f.Calls)
	}
	if strings.Contains(logs.String(), "recorded as no verdict") {
		t.Fatalf("the node recorded a no-verdict of its own:\n%s", logs.String())
	}
}
