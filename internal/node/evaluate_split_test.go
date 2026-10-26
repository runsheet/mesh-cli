package node

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// splitFixture is an evaluator on item 1, in evaluating on this node,
// with the given evaluations already recorded against it (each notes
// string one record; attempts as the platform would count them) and an
// evaluator whose verdict fails.
func splitFixture(t *testing.T, attempts int, records ...string) (*gatewaytest.Fake, *Evaluator, *bytes.Buffer) {
	t.Helper()
	f := seed(t)
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	gw := gateway.New(f.URL(), "rs_test")
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	var evals []any
	for i, notes := range records {
		evals = append(evals, map[string]any{"attempt": i + 1, "notes": notes, "failedClauses": []any{}})
	}
	f.WorkItems[item1]["evaluations"] = evals
	if attempts > 0 {
		f.WorkItems[item1]["attempts"] = attempts
	}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "RegisterNode does not pin the aggregate id.\n\nVERDICT: FAIL"}
	var logs bytes.Buffer
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MaxBudgetUSD: 2, Splits: &OncePer{}, Log: log.New(&logs, "", 0)}
	return f, ev, &logs
}

// failItem1 runs the fixture's evaluation and checks it recorded a
// failed verdict.
func failItem1(t *testing.T, f *gatewaytest.Fake, ev *Evaluator) {
	t.Helper()
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) == 0 || calls[len(calls)-1].Input["passed"] != false {
		t.Fatalf("no failed verdict recorded: %+v", calls)
	}
}

// An item with one failed verdict already recorded that fails again asks
// for a split after the verdict is handed in: one requestPlan replacing
// the item (ADR-021's amendment 2026-10-02), naming no one as `by`
// (runsheet ADR 070).
func TestSecondFailedEvaluationAsksForASplit(t *testing.T) {
	f, ev, logs := splitFixture(t, 1, "the aggregate id is not NodeID(owner, slug)\n\n(evaluated on Claude opus)")
	failItem1(t, f, ev)
	reqs := f.RequestPlans()
	if len(reqs) != 1 || reqs[0].ReplaceItem != item1 || reqs[0].GoalID != goalID || reqs[0].By != "" {
		t.Fatalf("requestPlan calls = %+v, want one replacing %s", reqs, item1)
	}
	if got := f.Goals[goalID]["replacingItemId"]; got != item1 {
		t.Fatalf("goal's replacingItemId = %v", got)
	}
	calls := f.Calls
	last := -1
	for i, c := range calls {
		if c.Name == "evaluateWorkItem" {
			last = i
		}
		if c.Name == "requestPlan" && last == -1 {
			t.Fatalf("requestPlan sent before the verdict: %+v", calls)
		}
	}
	if !strings.Contains(logs.String(), "item ship-it/1: failed evaluation 2; asking for a split (narrow re-plan) instead of a third build") {
		t.Fatalf("log lacks the split line:\n%s", logs.String())
	}
}

// The first failure is the platform's rework: nothing is asked.
func TestFirstFailedEvaluationAsksNothing(t *testing.T) {
	f, ev, logs := splitFixture(t, 0)
	failItem1(t, f, ev)
	if reqs := f.RequestPlans(); len(reqs) != 0 {
		t.Fatalf("requestPlan calls = %+v, want none", reqs)
	}
	if strings.Contains(logs.String(), "split") {
		t.Fatalf("log mentions a split:\n%s", logs.String())
	}
}

// The third failure is the platform's own narrow re-plan (runsheet ADR
// 023, rung 3): the node asks nothing.
func TestThirdFailedEvaluationAsksNothing(t *testing.T) {
	f, ev, _ := splitFixture(t, 2, "first: fails", "second: fails again")
	failItem1(t, f, ev)
	if reqs := f.RequestPlans(); len(reqs) != 0 {
		t.Fatalf("requestPlan calls = %+v, want none", reqs)
	}
}

// A record of an evaluation that reached no verdict is not a failure
// (runsheet ADR 035): with it and this one failure the count is 1.
func TestNoVerdictDoesNotCountTowardTheSplit(t *testing.T) {
	f, ev, _ := splitFixture(t, 1, "no VERDICT line: I could not read the diff")
	failItem1(t, f, ev)
	if reqs := f.RequestPlans(); len(reqs) != 0 {
		t.Fatalf("requestPlan calls = %+v, want none", reqs)
	}
}

// A platform that refuses the split still has the verdict: the
// evaluation succeeds, and the refusal is a log line, once per item.
func TestSplitRefusedIsLoggedNotFailed(t *testing.T) {
	f, ev, logs := splitFixture(t, 1, "first: fails")
	f.Refuse["requestPlan"] = "item 1 re-landed 3 times; a person decides now"
	failItem1(t, f, ev)
	if reqs := f.RequestPlans(); len(reqs) != 1 || reqs[0].ReplaceItem != item1 {
		t.Fatalf("requestPlan calls = %+v", reqs)
	}
	if row := f.Item(item1); row["status"] != "ready" {
		t.Fatalf("item = %+v, want the failed verdict recorded (ready)", row)
	}
	want := "item ship-it/1: split refused by the platform: item 1 re-landed 3 times; a person decides now; the platform's ladder continues"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log lacks %q:\n%s", want, logs.String())
	}
	// said once per item
	ev.askSplit(context.Background(), gateway.WorkItem{ID: item1, GoalID: goalID, OrgID: orgID}, "ship-it/1", []harness.PriorAttempt{{Attempt: 1, Notes: "first: fails"}})
	if reqs := f.RequestPlans(); len(reqs) != 2 {
		t.Fatalf("requestPlan calls = %+v, want the second ask sent", reqs)
	}
	if n := strings.Count(logs.String(), "split refused"); n != 1 {
		t.Fatalf("split refused said %d times:\n%s", n, logs.String())
	}
}

// A gateway whose RequestPlanInput has no replaceItem is sent nothing,
// and nothing is logged as an error.
func TestSplitNotSentToAnOlderGateway(t *testing.T) {
	f, ev, logs := splitFixture(t, 1, "first: fails")
	f.NoReplaceItem = true
	failItem1(t, f, ev)
	if reqs := f.RequestPlans(); len(reqs) != 0 {
		t.Fatalf("requestPlan calls = %+v, want none", reqs)
	}
	if n := len(f.ReadsNamed("__type RequestPlanInput")); n != 1 {
		t.Fatalf("RequestPlanInput asked %d times, want 1", n)
	}
	for _, bad := range []string{"split", "error", "refused"} {
		if strings.Contains(logs.String(), bad) {
			t.Fatalf("log carries %q:\n%s", bad, logs.String())
		}
	}
}
