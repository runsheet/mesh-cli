package gateway_test

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const (
	actorItem = "dddddddd-4444-4444-8444-dddddddddddd"
	actorGoal = "eeeeeeee-5555-4555-8555-eeeeeeeeeeee"
)

// actorCommands are the commands that carry the actor, in the order a
// session sends them, each with the item status the fake needs first.
var actorCommands = []struct {
	name   string
	field  string
	status string
	send   func(ctx context.Context, gw *gateway.Client) error
}{
	{"claim", "claimWorkItem", "ready", func(ctx context.Context, gw *gateway.Client) error {
		return gw.ClaimWorkItem(ctx, actorItem, "node-1", "claude-code")
	}},
	{"progress", "reportWorkItemProgress", "claimed", func(ctx context.Context, gw *gateway.Client) error {
		return gw.ReportWorkItemProgress(ctx, actorItem, "working", 10, "sess-1")
	}},
	{"finish", "finishWorkItem", "running", func(ctx context.Context, gw *gateway.Client) error {
		return gw.FinishWorkItem(ctx, actorItem, "mesh/g-1", "abc123", 20, nil)
	}},
	{"finish already done", "finishWorkItem", "running", func(ctx context.Context, gw *gateway.Client) error {
		return gw.FinishWorkItemAlreadyDone(ctx, actorItem, "on main at abc123", 5, nil)
	}},
	{"verdict", "evaluateWorkItem", "evaluating", func(ctx context.Context, gw *gateway.Client) error {
		return gw.EvaluateWorkItem(ctx, actorItem, true, "VERDICT: PASS", nil, 30, nil, gateway.EvaluateChoice{})
	}},
	{"no verdict", "evaluateWorkItem", "evaluating", func(ctx context.Context, gw *gateway.Client) error {
		return gw.EvaluateWorkItemNoVerdict(ctx, actorItem, "no VERDICT line", 30, nil, gateway.EvaluateChoice{})
	}},
	{"plan", "planGoal", "", func(ctx context.Context, gw *gateway.Client) error {
		return gw.PlanGoal(ctx, actorGoal, "node-1", []gateway.PlannedItem{{Seq: 1, Title: "A", Acceptance: "go test ./..."}}, 40, nil)
	}},
}

func actorFake(t *testing.T) *gatewaytest.Fake {
	t.Helper()
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	f.WorkItems[actorItem] = map[string]any{"id": actorItem, "orgId": orgID, "seq": 3, "status": "ready", "nodeId": "node-1"}
	f.Goals[actorGoal] = map[string]any{"id": actorGoal, "orgId": orgID, "slug": "g", "status": "planning"}
	return f
}

// sendAll sends every actor command on ctx, each from the status the
// fake needs, and fails the test on any error.
func sendAll(t *testing.T, f *gatewaytest.Fake, gw *gateway.Client, ctx context.Context) {
	t.Helper()
	for _, c := range actorCommands {
		if c.status != "" {
			f.WorkItems[actorItem]["status"] = c.status
		}
		if err := c.send(ctx, gw); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

// fiveCalls are every recorded call of the five actor commands.
func fiveCalls(f *gatewaytest.Fake) []gatewaytest.Call {
	var calls []gatewaytest.Call
	for _, n := range []string{"claimWorkItem", "reportWorkItemProgress", "finishWorkItem", "evaluateWorkItem", "planGoal"} {
		calls = append(calls, f.CallsNamed(n)...)
	}
	return calls
}

// A session's commands carry actor agent:<role>@<slug> for the role its
// ctx names, on claim, progress, finish, verdict and plan, each input
// type introspected once however many commands are sent.
func TestActorRidesTheSessionsCommands(t *testing.T) {
	for _, role := range []gateway.Role{gateway.RoleBuilder, gateway.RoleEvaluator, gateway.RolePlanner} {
		t.Run(string(role), func(t *testing.T) {
			f := actorFake(t)
			gw := gateway.New(f.URL(), "rs_test")
			ctx := gateway.WithActor(context.Background(), role, "mini")
			want := "agent:" + string(role) + "@mini"
			if got, ok := gateway.ActorFrom(ctx); !ok || got != want {
				t.Fatalf("ActorFrom = %q, %v; want %q", got, ok, want)
			}
			sendAll(t, f, gw, ctx)
			calls := fiveCalls(f)
			if len(calls) != len(actorCommands) {
				t.Fatalf("%d calls recorded, want %d: %+v", len(calls), len(actorCommands), calls)
			}
			for _, c := range calls {
				if got := c.Input[gateway.ActorField]; got != want {
					t.Errorf("%s: actor = %#v, want %q", c.Name, got, want)
				}
			}
			// EvaluateWorkItemInput is asked twice: once for actor, once
			// for the no-verdict path's own noVerdict probe
			for typ, want := range map[string]int{"ClaimWorkItemInput": 1, "ReportWorkItemProgressInput": 1, "FinishWorkItemInput": 1, "EvaluateWorkItemInput": 2, "PlanGoalInput": 1} {
				if n := len(f.ReadsNamed("__type " + typ)); n != want {
					t.Errorf("%s asked %d times, want %d", typ, n, want)
				}
			}
		})
	}
}

// A platform whose inputs have no `actor` is sent none — the probe says
// so, and the fake would refuse the key — and no command errors.
func TestActorLeftOffAnOlderPlatform(t *testing.T) {
	for _, role := range []gateway.Role{gateway.RoleBuilder, gateway.RoleEvaluator, gateway.RolePlanner} {
		t.Run(string(role), func(t *testing.T) {
			f := actorFake(t)
			f.NoActor = true
			gw := gateway.New(f.URL(), "rs_test")
			sendAll(t, f, gw, gateway.WithActor(context.Background(), role, "mini"))
			for _, c := range fiveCalls(f) {
				if _, sent := c.Input[gateway.ActorField]; sent {
					t.Errorf("%s sent actor to a platform without it: %+v", c.Name, c.Input)
				}
			}
		})
	}
}

// The probe is kept for ScalingProbeEvery and then asked again: a
// platform that deploys the field is sent the actor after the hour.
func TestActorProbeKeptAnHour(t *testing.T) {
	f := actorFake(t)
	f.NoActor = true
	f.WorkItems[actorItem]["status"] = "claimed"
	gw := gateway.New(f.URL(), "rs_test")
	now := time.Now()
	gw.Now = func() time.Time { return now }
	ctx := gateway.WithActor(context.Background(), gateway.RoleBuilder, "mini")
	if err := gw.ReportWorkItemProgress(ctx, actorItem, "a", 0, ""); err != nil {
		t.Fatal(err)
	}
	f.NoActor = false
	if err := gw.ReportWorkItemProgress(ctx, actorItem, "b", 0, ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(gateway.ScalingProbeEvery + time.Second)
	if err := gw.ReportWorkItemProgress(ctx, actorItem, "c", 0, ""); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("reportWorkItemProgress")
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	for i, want := range []any{nil, nil, "agent:builder@mini"} {
		if got := calls[i].Input[gateway.ActorField]; got != want {
			t.Errorf("call %d: actor = %#v, want %#v", i, got, want)
		}
	}
	if n := len(f.ReadsNamed("__type ReportWorkItemProgressInput")); n != 2 {
		t.Errorf("asked %d times, want 2", n)
	}
}

// A ctx without WithActor — the operator's commands, `mesh mcp`'s
// pass-through work tools acting as the person — sends no actor and
// asks nothing about it.
func TestNoActorWithoutWithActor(t *testing.T) {
	f := actorFake(t)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	if _, ok := gateway.ActorFrom(ctx); ok {
		t.Fatal("ActorFrom answered on a bare ctx")
	}
	sendAll(t, f, gw, ctx)
	for _, c := range fiveCalls(f) {
		if _, sent := c.Input[gateway.ActorField]; sent {
			t.Errorf("%s sent actor with no WithActor: %+v", c.Name, c.Input)
		}
	}
	for _, typ := range []string{"ClaimWorkItemInput", "ReportWorkItemProgressInput", "FinishWorkItemInput"} {
		if n := len(f.ReadsNamed("__type " + typ)); n != 0 {
			t.Errorf("%s asked %d times with no actor, want 0", typ, n)
		}
	}
}
