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

// notYoursFixture is an evaluator on item 1, in evaluating on this node,
// with a harness that would pass it if it ever ran.
func notYoursFixture(t *testing.T) (*gatewaytest.Fake, *Evaluator, *bytes.Buffer) {
	t.Helper()
	f := seed(t)
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	gw := gateway.New(f.URL(), "rs_test")
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "Looks right.\n\nVERDICT: PASS"}
	var logs bytes.Buffer
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MaxBudgetUSD: 2, Log: log.New(&logs, "", 0)}
	return f, ev, &logs
}

// A node whose issueItemRepoToken is refused "not yours" — another node
// already converged on this item's evaluation (ADR-009) — leaves it to
// that node quietly: no session, no verdict, no release of the claim.
func TestEvaluateTokenRefusedNotYoursLeavesItToTheOtherNode(t *testing.T) {
	f, ev, logs := notYoursFixture(t)
	f.Refuse["issueItemRepoToken"] = "issueItemRepoToken: you are not the node evaluating the item: not yours"
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatalf("Evaluate returned an error, want nil: %v", err)
	}
	if h := ev.Harness.(*fakeHarness); len(h.jobs) != 0 {
		t.Fatalf("a session was started: %+v", h.jobs)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 0 {
		t.Fatalf("a verdict was sent: %+v", calls)
	}
	if row := f.Item(item1); row["status"] != "evaluating" {
		t.Fatalf("the claim was touched: %+v", row)
	}
	want := "item ship-it/1: evaluation is another node's (github token: issueItemRepoToken: you are not the node evaluating the item: not yours); left to it"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log lacks %q:\n%s", want, logs.String())
	}
	if strings.Contains(logs.String(), "evaluate on") {
		t.Fatalf("log carries an \"evaluate on\" line:\n%s", logs.String())
	}
}

// A node whose closing finishJob is refused "not yours" has lost the
// race to another node after its own session already ran: the close is
// dropped, not retried forever, and the item is forgotten rather than
// held for a next note.
func TestEvaluateCloseRefusedNotYoursIsNotRetried(t *testing.T) {
	f, ev, logs := notYoursFixture(t)
	f.EvaluationNotHandedIn = "finishJob: you are not the node evaluating the item: not yours"
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatalf("Evaluate returned an error, want nil: %v", err)
	}
	if n := len(f.CallsNamed("evaluateWorkItem")); n != 0 {
		t.Fatalf("the node sent %d verdicts of its own", n)
	}
	want := "item ship-it/1: evaluation closed elsewhere: another node holds it"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log lacks %q:\n%s", want, logs.String())
	}
}
