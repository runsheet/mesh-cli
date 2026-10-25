package gateway_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// evaluatingItem is finishItem, finished and waiting for its evaluation.
func evaluatingItem(f *gatewaytest.Fake) {
	f.WorkItems[finishItem] = map[string]any{"id": finishItem, "orgId": orgID, "seq": 7, "status": "evaluating"}
}

// A gateway whose EvaluateWorkItemInput has `noVerdict` takes a no-verdict
// evaluation as runsheet ADR 035 defines it: passed false, noVerdict true,
// the reason as noVerdictReason and in notes, the cost, and never
// `outcome`. The input type is asked about once, however many are sent.
func TestEvaluateWorkItemNoVerdictSendsTheOutcome(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := nodeCtx()
	for range 2 {
		if err := gw.EvaluateWorkItemNoVerdict(ctx, finishItem, "no VERDICT line: I could not tell.", 30, nil, gateway.EvaluateChoice{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.ReadsNamed("__type EvaluateWorkItemInput")); n != 1 {
		t.Fatalf("EvaluateWorkItemInput asked %d times, want 1", n)
	}
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	const reason = "no VERDICT line: I could not tell."
	in := calls[0].Input
	want := map[string]any{"passed": false, "noVerdict": true, "noVerdictReason": reason, "notes": reason, "costCents": float64(30)}
	for k, v := range want {
		if in[k] != v {
			t.Fatalf("%s = %#v, want %#v; input %+v", k, in[k], v, in)
		}
	}
	for k := range in {
		if _, ok := want[k]; !ok && k != "aggregateId" && k != "namespace" {
			t.Fatalf("unexpected field %q sent: %+v", k, in)
		}
	}
	if _, sent := in["outcome"]; sent {
		t.Fatalf("a no-verdict evaluation sent outcome: %+v", in)
	}
	if row := f.Item(finishItem); row["status"] != "evaluating" || row["noVerdicts"] != 2 || row["noVerdictReason"] != reason || row["attempts"] != nil {
		t.Fatalf("row = %+v", row)
	}
}

// A gateway whose EvaluateWorkItemInput lacks `noVerdict` is sent nothing:
// ErrNoVerdictOutcome, asked once and kept for ScalingProbeEvery, then
// asked again and — the platform having deployed it — sent.
func TestEvaluateWorkItemNoVerdictOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoNoVerdict = true
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	now := time.Now()
	gw.Now = func() time.Time { return now }
	ctx := nodeCtx()
	for range 2 {
		if err := gw.EvaluateWorkItemNoVerdict(ctx, finishItem, "blind", 30, nil, gateway.EvaluateChoice{}); !errors.Is(err, gateway.ErrNoVerdictOutcome) {
			t.Fatalf("err = %v, want ErrNoVerdictOutcome", err)
		}
	}
	if n := len(f.ReadsNamed("__type EvaluateWorkItemInput")); n != 1 {
		t.Fatalf("EvaluateWorkItemInput asked %d times, want 1", n)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 0 {
		t.Fatalf("sent to a platform without the field: %+v", calls)
	}

	f.NoNoVerdict = false
	now = now.Add(gateway.ScalingProbeEvery)
	if err := gw.EvaluateWorkItemNoVerdict(ctx, finishItem, "blind", 30, nil, gateway.EvaluateChoice{}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.ReadsNamed("__type EvaluateWorkItemInput")); n != 2 {
		t.Fatalf("EvaluateWorkItemInput asked %d times, want 2", n)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 1 || calls[0].Input["noVerdict"] != true {
		t.Fatalf("calls = %+v", calls)
	}
}

// A verdict carries no outcome, as before.
func TestEvaluateWorkItemCarriesNoOutcome(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.EvaluateWorkItem(nodeCtx(), finishItem, false, "no", nil, 10, nil, gateway.EvaluateChoice{}); err != nil {
		t.Fatal(err)
	}
	in := f.CallsNamed("evaluateWorkItem")[0].Input
	if _, sent := in["outcome"]; sent || in["passed"] != false {
		t.Fatalf("input = %+v", in)
	}
}

// The fake refuses what the platform refuses (runsheet ADR 035): a
// no-verdict that passes, or one with a blank reason, and `outcome`.
func TestTheFakeRefusesANoVerdictThePlatformRefuses(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := nodeCtx()
	if err := gw.EvaluateWorkItemNoVerdict(ctx, finishItem, "  ", 30, nil, gateway.EvaluateChoice{}); err == nil {
		t.Fatal("a blank reason was taken")
	}
	for _, in := range []map[string]any{
		{"passed": true, "noVerdict": true, "noVerdictReason": "blind"},
		{"outcome": "no_verdict", "notes": "blind"},
		{"noVerdict": true, "noVerdictReason": "blind"},
	} {
		if err := gw.Mutate(ctx, "evaluateWorkItem", finishItem, in); err == nil {
			t.Fatalf("%+v was taken", in)
		}
	}
	if row := f.Item(finishItem); row["status"] != "evaluating" || row["noVerdicts"] != nil {
		t.Fatalf("row = %+v", row)
	}
	// the outcome shape is outside runsheet's contract too: the fake
	// refused it there before the aggregate saw it
	for _, v := range f.TakeContractViolations() {
		if !strings.Contains(v, "outcome: not in the contract") {
			t.Errorf("violation %q, want only the outcome one", v)
		}
	}
}
