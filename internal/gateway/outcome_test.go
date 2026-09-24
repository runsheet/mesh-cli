package gateway_test

import (
	"context"
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
	if err := gw.FinishWorkItemAlreadyDone(context.Background(), finishItem, ev, 40); err != nil {
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
	if err := gw.FinishWorkItemAlreadyDone(context.Background(), finishItem, "  ", 0); err == nil {
		t.Fatal("an already_done finish without evidence must be refused before it is sent")
	}
}

// A finish without an outcome is built: the input is the one it always
// was, which a platform without outcomes still takes.
func TestFinishWorkItemBuiltUnchanged(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoOutcome = true
	runningItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	if err := gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc123", 12); err != nil {
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

// A WorkItem decodes its outcome and evidence; one without them — or
// from a platform that cannot select them — is built.
func TestWorkItemOutcomeDecodes(t *testing.T) {
	ctx := context.Background()
	for _, old := range []bool{false, true} {
		f := gatewaytest.New("rs_test")
		f.NoOutcome = old
		f.WorkItems["item-1"] = map[string]any{"id": "item-1", "orgId": orgID, "goalId": "g", "seq": 1, "status": "merged"}
		f.WorkItems["item-2"] = map[string]any{"id": "item-2", "orgId": orgID, "goalId": "g", "seq": 2, "status": "merged"}
		if !old {
			f.WorkItems["item-2"]["outcome"], f.WorkItems["item-2"]["evidence"] = "already_done", "PR #3"
		}
		gw := gateway.New(f.URL(), "rs_test")

		items, err := gw.GoalWorkItems(ctx, "g")
		if err != nil {
			t.Fatalf("old=%v: %v", old, err)
		}
		if len(items) != 2 || items[0].Outcome != gateway.OutcomeBuilt || items[0].Evidence != "" {
			t.Fatalf("old=%v: items = %+v", old, items)
		}
		want2 := gateway.OutcomeAlreadyDone
		if old {
			want2 = gateway.OutcomeBuilt // the read is asked again without the fields
		}
		if items[1].Outcome != want2 {
			t.Fatalf("old=%v: item 2 outcome = %q, want %q", old, items[1].Outcome, want2)
		}
		one, err := gw.WorkItem(ctx, "item-2")
		if err != nil {
			t.Fatalf("old=%v: %v", old, err)
		}
		if !old && (one.Outcome != gateway.OutcomeAlreadyDone || one.Evidence != "PR #3") {
			t.Fatalf("item-2 = %+v", one)
		}
		f.Close()
	}
}
