package node

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// priorAttemptsFixture is an evaluator on item 1, already evaluating, with
// two evaluations already recorded against it in the fake: the first
// failed on a clause, the second failed with none.
func priorAttemptsFixture(t *testing.T) (*gatewaytest.Fake, *Evaluator, *fakeHarness, *bytes.Buffer) {
	t.Helper()
	f := seed(t)
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"

	clauses := []gateway.FailedClause{{Clause: "the three shards' durations are within 20% of each other", Satisfy: "a reading recorded honestly under load"}}
	if err := gw.EvaluateWorkItem(ctx, item1, false, "first attempt: fails on load", clauses, 10, nil, gateway.EvaluateChoice{}); err != nil {
		t.Fatal(err)
	}
	if err := gw.EvaluateWorkItem(ctx, item1, false, "second attempt: fails again", nil, 10, nil, gateway.EvaluateChoice{}); err != nil {
		t.Fatal(err)
	}
	// the retry's build lands it back in evaluating
	f.WorkItems[item1]["status"] = "evaluating"

	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "Holds.\n\nVERDICT: PASS"}
	var logs bytes.Buffer
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MaxBudgetUSD: 2, Log: log.New(&logs, "", 0)}
	return f, ev, h, &logs
}

// A retry's evaluate prompt carries the lineage's previous verdicts: the
// evaluator reads them with WorkItemPriorAttempts before building the
// prompt, so a narrow re-plan is judged on what earlier attempts failed
// on, not a fresh reading of the acceptance.
func TestEvaluatePromptCarriesEarlierAttempts(t *testing.T) {
	f, ev, h, _ := priorAttemptsFixture(t)
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(h.jobs))
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{
		"# What earlier attempts failed on",
		"## Attempt 1",
		"the three shards' durations are within 20% of each other",
		"a reading recorded honestly under load",
		"## Attempt 2",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if len(f.CallsNamed("evaluateWorkItem")) != 3 {
		t.Fatalf("calls = %+v", f.CallsNamed("evaluateWorkItem"))
	}
}

// A platform without `evaluations` on WorkItemIndex (NoEvaluations) still
// lets the evaluation run and record a verdict — the lineage is simply
// unread, logged once rather than failing the item.
func TestEvaluateToleratesNoEvaluations(t *testing.T) {
	f, ev, h, logs := priorAttemptsFixture(t)
	f.NoEvaluations = true
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(h.jobs))
	}
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 3 || calls[2].Input["passed"] != true {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.Contains(logs.String(), "earlier evaluations unread") {
		t.Fatalf("log lacks the unread notice:\n%s", logs.String())
	}
	if strings.Contains(h.jobs[0].Prompt, "# What earlier attempts failed on") {
		t.Fatalf("prompt carries attempts it could not read:\n%s", h.jobs[0].Prompt)
	}
}
