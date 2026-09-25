package node

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// An item this node finished sits in evaluating; the tick hands it to
// the evaluator, whose fresh session sees the diff and the acceptance
// and whose verdict line is recorded. No verdict, nothing recorded.
func TestLoopEvaluatesFinishedItems(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	if err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node"}); err != nil {
		t.Fatal(err)
	}
	// item 1 finished here; item 2 is another node's, not ours to judge
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	f.WorkItems[item2]["status"] = "evaluating"
	f.WorkItems[item2]["nodeId"] = "33333333-3333-4333-8333-333333333333"
	f.WorkItems[item2]["branch"] = "mesh/ship-it-2"

	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "The test exists and fails for the right reason.\n\nVERDICT: PASS"}
	ev := &Evaluator{GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MCP: PrologueMCP(f.URL(), "rs_test"), MaxBudgetUSD: 2, Log: log.New(os.Stderr, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: ev.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Evaluate: func(ctx context.Context, item gateway.WorkItem) { _ = ev.Evaluate(ctx, item) }}
	// an evaluation takes no slot; none lets a tick claim the item a FAIL readies
	cfg.Capacity = Fixed(0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	waitFor(t, func() bool { return f.Item(item1)["status"] == "review" })

	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 1 || calls[0].Input["aggregateId"] != item1 || calls[0].Input["passed"] != true || calls[0].Input["notes"] != "The test exists and fails for the right reason.\n\n(evaluated on the CLI's default model)" || calls[0].Input["costCents"] != float64(125) {
		t.Fatalf("evaluate calls: %+v", calls)
	}
	if f.Item(item2)["status"] != "evaluating" {
		t.Fatal("another node's item is not ours to evaluate")
	}
	if len(ws.prepared) != 1 || ws.prepared[0].Branch != "mesh/ship-it-1" || ws.prepared[0].BaseRef != "main" {
		t.Fatalf("prepared: %+v", ws.prepared)
	}
	if len(ws.pushed) != 0 {
		t.Fatal("the evaluator changes nothing")
	}
	if len(h.jobs) != 1 || h.jobs[0].Kind != harness.KindEvaluate || h.jobs[0].MaxBudgetUSD != 2 || h.jobs[0].MCPServers["prologue"] == nil {
		t.Fatalf("job: %+v", h.jobs)
	}
	if _, err := os.Stat(filepath.Join(ws.prepared[0].Dir(), ContextDir, "adr-adr-001.md")); err != nil {
		t.Fatal("the evaluator must have the ADRs on disk")
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{"Write the failing test", "the test fails", "go test ./... passes", "+func TestThing", "origin/main...HEAD", harness.VerdictPass, harness.VerdictFail, "Change nothing",
		".mesh/context/", "adr-adr-001.md", "goes against an ADR", "prologue_document", `project "acme/mesh"`} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	// a second tick finds nothing of ours left to evaluate
	loop.Tick(ctx)
	loop.wg.Wait()
	if len(h.jobs) != 1 {
		t.Fatalf("a reviewed item was evaluated again: %d jobs", len(h.jobs))
	}

	// a failing verdict readies it again with the notes; no verdict leaves it
	f.WorkItems[item1]["status"] = "evaluating"
	h.text = "The test passes already, so it proves nothing.\nVERDICT: FAIL"
	loop.Tick(ctx)
	waitFor(t, func() bool { return f.Item(item1)["status"] == "ready" })
	if f.Item(item1)["evaluation"] != "The test passes already, so it proves nothing.\n\n(evaluated on the CLI's default model)" {
		t.Fatalf("notes: %v", f.Item(item1)["evaluation"])
	}
	f.WorkItems[item1]["status"] = "evaluating"
	h.text = "I could not tell."
	loop.Tick(ctx)
	loop.wg.Wait()
	if f.Item(item1)["status"] != "evaluating" || len(f.CallsNamed("evaluateWorkItem")) != 2 {
		t.Fatalf("no verdict must record nothing: %v", f.Item(item1)["status"])
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		in     string
		passed bool
		notes  string
		ok     bool
	}{
		{"all good\n\nVERDICT: PASS", true, "all good", true},
		{"missing the test\n**VERDICT: FAIL**", false, "missing the test", true},
		{"VERDICT: PASS\nbut then again\nverdict: fail", false, "VERDICT: PASS\nbut then again", true},
		{"no idea", false, "", false},
		{"", false, "", false},
	}
	for _, c := range cases {
		passed, notes, ok := harness.ParseVerdict(c.in)
		if passed != c.passed || notes != c.notes || ok != c.ok {
			t.Errorf("ParseVerdict(%q) = %v %q %v", c.in, passed, notes, ok)
		}
	}
}

// A retry's build prompt carries the evaluator's notes.
func TestBuildPromptOnRetry(t *testing.T) {
	p := harness.BuildPrompt(harness.PromptInput{GoalTitle: "Ship it", Seq: 1, Title: "T", Attempts: 1, Evaluation: "the test proves nothing"})
	if !strings.Contains(p, "attempt 2") || !strings.Contains(p, "the test proves nothing") {
		t.Fatalf("retry prompt:\n%s", p)
	}
	if strings.Contains(harness.BuildPrompt(harness.PromptInput{GoalTitle: "Ship it", Seq: 1, Title: "T"}), "Previous attempt") {
		t.Fatal("a first attempt has no previous one")
	}
}
