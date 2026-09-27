package gateway_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const finishItem = "cccccccc-3333-4333-8333-cccccccccccc"

func runningItem(f *gatewaytest.Fake) {
	f.WorkItems[finishItem] = map[string]any{"id": finishItem, "orgId": orgID, "seq": 7, "status": "running"}
}

// An already_done finish (mesh ADR 003 decision 1) sends the outcome and
// the evidence, and no branch or head.
func TestFinishWorkItemAlreadyDone(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	runningItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	ev := "https://github.com/acme/widgets/pull/3 merged item 3's work; abc123 on main"
	if err := gw.FinishWorkItemAlreadyDone(context.Background(), finishItem, ev, 40, nil); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("finishWorkItem")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	in := calls[0].Input
	if in["outcome"] != "already_done" || in["evidence"] != ev || in["costCents"] != float64(40) {
		t.Fatalf("input = %+v", in)
	}
	for _, k := range []string{"branch", "headSha"} {
		if _, ok := in[k]; ok {
			t.Fatalf("an already_done finish sent %s: %+v", k, in)
		}
	}
	if row := f.Item(finishItem); row["status"] != "evaluating" || row["outcome"] != "already_done" {
		t.Fatalf("row = %+v", row)
	}
	if err := gw.FinishWorkItemAlreadyDone(context.Background(), finishItem, "  ", 0, nil); err == nil {
		t.Fatal("an already_done finish without evidence must be refused before it is sent")
	}
}

// A finish without an outcome is built: the input is the one it always
// was, which a platform without outcomes still takes.
func TestFinishWorkItemBuiltUnchanged(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields = map[string]bool{"outcome": true, "evidence": true}
	runningItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	if err := gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc123", 12, nil); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("finishWorkItem")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	want := map[string]any{"aggregateId": finishItem, "namespace": "runsheet", "branch": "mesh/g-7", "headSha": "abc123", "costCents": float64(12)}
	if !reflect.DeepEqual(calls[0].Input, want) {
		t.Fatalf("input = %+v, want %+v", calls[0].Input, want)
	}
}

// A WorkItem decodes its outcome and evidence; one without them is
// built. The reads do not select them until schema.json carries them
// (ADR 010), so an item read from today's platform is built.
func TestWorkItemOutcomeDecodes(t *testing.T) {
	var done gateway.WorkItem
	if err := json.Unmarshal([]byte(`{"id":"item-2","outcome":"already_done","evidence":"PR #3"}`), &done); err != nil {
		t.Fatal(err)
	}
	if done.Outcome != gateway.OutcomeAlreadyDone || done.Evidence != "PR #3" {
		t.Fatalf("item-2 = %+v", done)
	}
	var built gateway.WorkItem
	if err := json.Unmarshal([]byte(`{"id":"item-1","seq":1,"status":"merged"}`), &built); err != nil {
		t.Fatal(err)
	}
	if built.Outcome != gateway.OutcomeBuilt || built.Evidence != "" || built.Seq != 1 || built.Status != "merged" {
		t.Fatalf("item-1 = %+v", built)
	}

	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields = map[string]bool{"outcome": true, "evidence": true}
	f.WorkItems["item-1"] = map[string]any{"id": "item-1", "orgId": orgID, "goalId": "g", "seq": 1, "status": "merged"}
	items, err := gateway.New(f.URL(), "rs_test").GoalWorkItems(context.Background(), "g")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Outcome != gateway.OutcomeBuilt {
		t.Fatalf("items = %+v", items)
	}
}
