package gateway_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// evaluatingItem is finishItem, finished and waiting for its evaluation.
func evaluatingItem(f *gatewaytest.Fake) {
	f.WorkItems[finishItem] = map[string]any{"id": finishItem, "orgId": orgID, "seq": 7, "status": "evaluating"}
}

// A gateway whose EvaluateWorkItemInput has `outcome` takes a no-verdict
// evaluation: the outcome, the reason in notes, the cost, and no
// `passed`. The input type is asked about once, however many are sent.
func TestEvaluateWorkItemNoVerdictSendsTheOutcome(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	for range 2 {
		if err := gw.EvaluateWorkItemNoVerdict(ctx, finishItem, "no VERDICT line: I could not tell.", 30, nil); err != nil {
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
	in := calls[0].Input
	if in["outcome"] != gateway.EvaluationNoVerdict || in["notes"] != "no VERDICT line: I could not tell." || in["costCents"] != float64(30) {
		t.Fatalf("input = %+v", in)
	}
	if _, sent := in["passed"]; sent {
		t.Fatalf("a no-verdict evaluation sent passed: %+v", in)
	}
	if row := f.Item(finishItem); row["status"] != "evaluating" {
		t.Fatalf("row = %+v", row)
	}
}

// A gateway whose EvaluateWorkItemInput lacks `outcome` is sent nothing:
// ErrNoVerdictOutcome, asked once and kept for ScalingProbeEvery, then
// asked again and — the platform having deployed it — sent.
func TestEvaluateWorkItemNoVerdictOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoEvaluationOutcome = true
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	now := time.Now()
	gw.Now = func() time.Time { return now }
	ctx := context.Background()
	for range 2 {
		if err := gw.EvaluateWorkItemNoVerdict(ctx, finishItem, "blind", 30, nil); !errors.Is(err, gateway.ErrNoVerdictOutcome) {
			t.Fatalf("err = %v, want ErrNoVerdictOutcome", err)
		}
	}
	if n := len(f.ReadsNamed("__type EvaluateWorkItemInput")); n != 1 {
		t.Fatalf("EvaluateWorkItemInput asked %d times, want 1", n)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 0 {
		t.Fatalf("sent to a platform without the field: %+v", calls)
	}

	f.NoEvaluationOutcome = false
	now = now.Add(gateway.ScalingProbeEvery)
	if err := gw.EvaluateWorkItemNoVerdict(ctx, finishItem, "blind", 30, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(f.ReadsNamed("__type EvaluateWorkItemInput")); n != 2 {
		t.Fatalf("EvaluateWorkItemInput asked %d times, want 2", n)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 1 || calls[0].Input["outcome"] != gateway.EvaluationNoVerdict {
		t.Fatalf("calls = %+v", calls)
	}
}

// A verdict carries no outcome, as before.
func TestEvaluateWorkItemCarriesNoOutcome(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.EvaluateWorkItem(context.Background(), finishItem, false, "no", 10, nil); err != nil {
		t.Fatal(err)
	}
	in := f.CallsNamed("evaluateWorkItem")[0].Input
	if _, sent := in["outcome"]; sent || in["passed"] != false {
		t.Fatalf("input = %+v", in)
	}
}
