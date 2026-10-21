//go:build e2e

package node

import (
	"context"
	"log"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// An evaluation whose suite runs past the platform's stall window keeps
// heartbeating and is never stalled (goal
// a-session-that-ends-says-so-and-a-long-one-stays-alive, runsheet ADR
// 018's amendment 2026-10-18). The fake's stall sweep runs with a 2s
// window; the evaluation, claimed by the loop with
// claimWorkItemEvaluation, heartbeats every 500ms while its session — a
// long suite — runs 6s, three windows. The item is never marked stalled,
// a heartbeat lands on the evaluating item at the interval throughout,
// claim to verdict, and the verdict is recorded. The same evaluation
// with no heartbeat inside the run is stalled, so the window is real.

const (
	e2eStallWindow = 2 * time.Second
	e2eBeatEvery   = 500 * time.Millisecond
	e2eSuiteRuns   = 3 * e2eStallWindow
)

// stallFixture is item 1 in evaluating, built by this node, on a fake
// whose stall sweep runs at e2eStallWindow, and a loop whose evaluator
// heartbeats every beat and whose session runs e2eSuiteRuns.
func stallFixture(t *testing.T, beat time.Duration) (*gatewaytest.Fake, *Loop, *syncBuffer) {
	t.Helper()
	f := seed(t)
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node",
		NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	gw := gateway.New(f.URL(), "rs_test")
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID,
		Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: 1, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	f.StallWindow = e2eStallWindow
	logs := &syncBuffer{}
	logger := log.New(logs, "", log.Lmicroseconds)
	ws := &fakeWS{root: t.TempDir()}
	h := &sleepyHarness{fakeHarness: &fakeHarness{text: "The suite ran green, all three shards.\n\nVERDICT: PASS"}, d: e2eSuiteRuns}
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), Cfg: cfg,
		WorkRoot: ws.root, MaxBudgetUSD: 2, HeartbeatEvery: beat, Log: logger}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Harness: "claude-code", Log: logger,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Plan:     func(context.Context, gateway.Goal, Scope) {},
		Research: func(context.Context, gateway.Direction, gateway.Project, Scope) {},
	}
	loop.Evaluate = func(ctx context.Context, item gateway.WorkItem) {
		if err := ev.Evaluate(ctx, item); err != nil && ctx.Err() == nil {
			logger.Printf("item %d %q: evaluation: %v", item.Seq, item.Title, err)
		}
	}
	// the stall clock starts now, as the finish into evaluating starts it
	f.StallsOf(item1)
	return f, loop, logs
}

func TestEvaluationOutlivesTheStallWindowE2E(t *testing.T) {
	f, loop, logs := stallFixture(t, e2eBeatEvery)
	loop.Tick(context.Background())
	loop.wg.Wait()

	if st := f.StallsOf(item1); len(st) != 0 {
		t.Errorf("item 1 was marked stalled: %+v\n%s", st, logs)
	}
	claims := f.CallsNamed("claimWorkItemEvaluation")
	verdicts := f.CallsNamed("evaluateWorkItem")
	if len(claims) != 1 || len(verdicts) != 1 {
		t.Fatalf("%d claim(s), %d verdict(s), want one each:\n%s", len(claims), len(verdicts), logs)
	}
	if verdicts[0].Input["passed"] != true || f.Item(item1)["status"] != "review" {
		t.Errorf("verdict %v, item %v: want passed, review", verdicts[0].Input, f.Item(item1)["status"])
	}
	if ran := verdicts[0].At.Sub(claims[0].At); ran < e2eSuiteRuns {
		t.Errorf("the evaluation ran %s, want past %s (three stall windows)", ran, e2eSuiteRuns)
	}
	// every gap — claim to the first beat, beat to beat, the last beat
	// to the verdict — at about the interval, and never near the window
	beats := f.CallsNamed("reportWorkItemProgress")
	if want := int(e2eSuiteRuns/e2eBeatEvery) - 2; len(beats) < want {
		t.Errorf("%d heartbeat(s) over %s at %s, want at least %d", len(beats), e2eSuiteRuns, e2eBeatEvery, want)
	}
	at := []time.Time{claims[0].At}
	for _, b := range beats {
		if b.Input["aggregateId"] != item1 || b.Input["costCents"] != float64(0) {
			t.Errorf("heartbeat input %v, want item 1 at 0 cents", b.Input)
		}
		at = append(at, b.At)
	}
	at = append(at, verdicts[0].At)
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap > 2*e2eBeatEvery {
			t.Errorf("gap %d of %s between heartbeats, want about %s (window %s)\n%s", i, gap, e2eBeatEvery, e2eStallWindow, logs)
		}
	}
	for _, b := range beats {
		if b.At.After(verdicts[0].At) {
			t.Errorf("a heartbeat at %s after the verdict at %s", b.At, verdicts[0].At)
		}
	}
}

// The control: the same evaluation heartbeating less often than the run
// is stalled by the fake's sweep — the window the test above outlives is
// a real one.
func TestEvaluationOutlivesTheStallWindowE2EControl(t *testing.T) {
	f, loop, logs := stallFixture(t, time.Hour)
	loop.Tick(context.Background())
	loop.wg.Wait()
	if st := f.StallsOf(item1); len(st) != 1 || st[0].Status != "evaluating" {
		t.Errorf("stalls %+v, want item 1 stalled once in evaluating\n%s", st, logs)
	}
}
