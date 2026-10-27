package node

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// An evaluation heartbeats its item from claim to verdict (runsheet ADR
// 018's amendment 2026-10-18): a ticker independent of the session's
// output, stopped on every way out, a refused beat a lost claim.

// beatGateway records, in order, every heartbeat the evaluator sends
// and every command that ends its hold on the item — the job's close,
// the node's own verdict or no-verdict, the release — and when, whether
// or not the platform took it.
type beatGateway struct {
	Gateway
	mu     sync.Mutex
	events []beatEvent
}

type beatEvent struct {
	name string
	note string
	at   time.Time
}

func (g *beatGateway) record(name, note string) {
	g.mu.Lock()
	g.events = append(g.events, beatEvent{name: name, note: note, at: time.Now()})
	g.mu.Unlock()
}

func (g *beatGateway) ReportWorkItemProgress(ctx context.Context, itemID, note string, costCents int, sessionID string) error {
	g.record("beat", note)
	return g.Gateway.ReportWorkItemProgress(ctx, itemID, note, costCents, sessionID)
}

func (g *beatGateway) EvaluateWorkItem(ctx context.Context, itemID string, passed bool, notes string, failed []gateway.FailedClause, costCents int, usage *gateway.UsageReport, choice gateway.EvaluateChoice) error {
	g.record("verdict", "")
	return g.Gateway.EvaluateWorkItem(ctx, itemID, passed, notes, failed, costCents, usage, choice)
}

func (g *beatGateway) EvaluateWorkItemNoVerdict(ctx context.Context, itemID, reason string, costCents int, usage *gateway.UsageReport, choice gateway.EvaluateChoice) error {
	g.record("no-verdict", "")
	return g.Gateway.EvaluateWorkItemNoVerdict(ctx, itemID, reason, costCents, usage, choice)
}

func (g *beatGateway) FinishJob(ctx context.Context, nodeID, kind string, subject gateway.JobSubject, finish map[string]any) (map[string]any, error) {
	g.record("close", "")
	return g.Gateway.FinishJob(ctx, nodeID, kind, subject, finish)
}

func (g *beatGateway) ReleaseWorkItem(ctx context.Context, itemID, nodeID, note string) error {
	g.record("release", "")
	return g.Gateway.ReleaseWorkItem(ctx, itemID, nodeID, note)
}

func (g *beatGateway) snapshot() []beatEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]beatEvent(nil), g.events...)
}

// beats is how many heartbeats were sent.
func (g *beatGateway) beats() int {
	n := 0
	for _, ev := range g.snapshot() {
		if ev.name == "beat" {
			n++
		}
	}
	return n
}

// assertNoBeatAfter fails when a heartbeat was sent after the first
// event named end, or after returned.
func (g *beatGateway) assertNoBeatAfter(t *testing.T, end string, returned time.Time) {
	t.Helper()
	ended := false
	for _, ev := range g.snapshot() {
		if ev.name == end {
			ended = true
		}
		if ev.name == "beat" && (ended || ev.at.After(returned)) {
			t.Errorf("a heartbeat %q after the %s: %+v", ev.note, end, g.snapshot())
			return
		}
	}
	if end != "" && !ended {
		t.Errorf("no %s sent: %+v", end, g.snapshot())
	}
}

// sleepyHarness is a fakeHarness whose sessions run for d (or until
// their ctx is done) before they answer: a long suite.
type sleepyHarness struct {
	*fakeHarness
	d time.Duration
}

func (h *sleepyHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	select {
	case <-time.After(h.d):
	case <-ctx.Done():
		return harness.Result{}, context.Cause(ctx)
	}
	return h.fakeHarness.Run(ctx, job, on)
}

// heartbeatFixture is item 1 in evaluating on this node and an evaluator
// on a recording gateway, heartbeating every 20ms, whose session runs d
// and answers text.
func heartbeatFixture(t *testing.T, d time.Duration, text string) (*gatewaytest.Fake, *beatGateway, *Evaluator, *syncBuffer) {
	t.Helper()
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	bg := &beatGateway{Gateway: gw}
	logs := &syncBuffer{}
	ws := &fakeWS{root: t.TempDir()}
	ev := &Evaluator{Credentials: testCreds(gw), GW: bg, Harness: &sleepyHarness{fakeHarness: &fakeHarness{text: text}, d: d}, WS: ws,
		Scopes: scopesOf(cfg), Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 2, HeartbeatEvery: 20 * time.Millisecond, Log: log.New(logs, "", 0)}
	return f, bg, ev, logs
}

// The heartbeat starts with the evaluation, beats through the session
// with the phase in its note and cents 0, and stops before the node
// closes the job: none after it, nor after Evaluate returns.
func TestEvaluationHeartbeatRunsClaimToClose(t *testing.T) {
	f, bg, ev, logs := heartbeatFixture(t, 300*time.Millisecond, "Holds.\n\nVERDICT: PASS")
	f.HandedInVerdicts = map[string]map[string]any{item1: {"passed": true, "notes": "Holds."}}
	if err := ev.Evaluate(context.Background(), gatewaytestItem(f, item1)); err != nil {
		t.Fatalf("evaluate: %v\n%s", err, logs)
	}
	returned := time.Now()
	time.Sleep(100 * time.Millisecond)
	if n := bg.beats(); n < 5 {
		t.Errorf("%d heartbeats over a 300ms session at 20ms, want at least 5: %+v", n, bg.snapshot())
	}
	bg.assertNoBeatAfter(t, "close", returned)
	sawSession := false
	for _, ev := range bg.snapshot() {
		if ev.name == "beat" && ev.note == "evaluation heartbeat: session" {
			sawSession = true
		}
	}
	if !sawSession {
		t.Errorf("no heartbeat named the session phase: %+v", bg.snapshot())
	}
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if c.Input["costCents"] != float64(0) {
			t.Errorf("a heartbeat carried cents: %v", c.Input)
		}
	}
	if f.Item(item1)["status"] != "review" {
		t.Errorf("item 1 = %v, want review", f.Item(item1)["status"])
	}
}

// A session that hands in no verdict still has the node close its job
// (refused by the product): the heartbeat stops before that close.
func TestEvaluationHeartbeatStopsAtAnUnhandedClose(t *testing.T) {
	f, bg, ev, logs := heartbeatFixture(t, 150*time.Millisecond, "I looked around but say nothing.")
	f.EvaluationNotHandedIn = "finishJob: call mesh_verdict first"
	_ = ev.Evaluate(context.Background(), gatewaytestItem(f, item1))
	returned := time.Now()
	time.Sleep(100 * time.Millisecond)
	if bg.beats() == 0 {
		t.Errorf("no heartbeat during the session: %+v\n%s", bg.snapshot(), logs)
	}
	bg.assertNoBeatAfter(t, "close", returned)
}

// A release — the evaluation's checkout could not take its token —
// stops the heartbeat before it goes.
func TestEvaluationHeartbeatStopsAtRelease(t *testing.T) {
	f, bg, ev, _ := heartbeatFixture(t, 0, "")
	item := gatewaytestItem(f, item1)
	ctx, hb := ev.startHeartbeat(context.Background(), item.ID, "ship-it/1", evalPhaseCheckout)
	defer hb.end()
	waitFor(t, func() bool { return bg.beats() >= 2 })
	_ = ev.releaseInfrastructure(ctx, item, "ship-it/1", &WorkspaceError{Repo: "acme/widgets", Path: "/nowhere", Err: errors.New("disk full")})
	returned := time.Now()
	time.Sleep(100 * time.Millisecond)
	bg.assertNoBeatAfter(t, "release", returned)
	for _, ev := range bg.snapshot() {
		if ev.name == "beat" && ev.note != "evaluation heartbeat: checkout" {
			t.Errorf("heartbeat note %q, want the checkout phase", ev.note)
		}
	}
}

// A cancelled ctx — the node shutting down — stops the heartbeat with
// the evaluation: none after Evaluate returns.
func TestEvaluationHeartbeatStopsOnCancel(t *testing.T) {
	f, bg, ev, _ := heartbeatFixture(t, time.Hour, "VERDICT: PASS")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ev.Evaluate(ctx, gatewaytestItem(f, item1)) }()
	waitFor(t, func() bool { return bg.beats() >= 3 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Evaluate did not return on a cancelled ctx")
	}
	returned := time.Now()
	time.Sleep(100 * time.Millisecond)
	bg.assertNoBeatAfter(t, "", returned)
	if len(f.CallsNamed("evaluateWorkItem")) != 0 {
		t.Errorf("a verdict sent for a cancelled evaluation")
	}
}

// A heartbeat the platform refuses — the item no longer evaluating — is
// logged with the gateway's answer and ends the evaluation as a lost
// claim: the session stopped, no verdict sent, no beat after.
func TestEvaluationHeartbeatRefusedIsALostClaim(t *testing.T) {
	f, bg, ev, logs := heartbeatFixture(t, time.Hour, "VERDICT: PASS")
	done := make(chan error, 1)
	go func() { done <- ev.Evaluate(context.Background(), gatewaytestItem(f, item1)) }()
	waitFor(t, func() bool { return bg.beats() >= 2 })
	f.SetItem(item1, map[string]any{"status": "ready", "nodeId": nil})
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Evaluate did not end on a refused heartbeat")
	}
	returned := time.Now()
	if !errors.Is(err, ErrClaimLost) {
		t.Errorf("Evaluate = %v, want ErrClaimLost", err)
	}
	out := logs.String()
	if !strings.Contains(out, "item ship-it/1: evaluation heartbeat refused: ") || !strings.Contains(out, "item is ready, not running") {
		t.Errorf("the refusal is not logged with the gateway's answer:\n%s", out)
	}
	if !strings.Contains(out, "item ship-it/1: claim lost (progress refused: ") || !strings.Contains(out, "evaluation stopped") {
		t.Errorf("no lost-claim line:\n%s", out)
	}
	time.Sleep(100 * time.Millisecond)
	bg.assertNoBeatAfter(t, "", returned)
	if len(f.CallsNamed("evaluateWorkItem")) != 0 {
		t.Errorf("a verdict sent after the claim was lost")
	}
}

// gatewaytestItem reads id back from the fake as the node would.
func gatewaytestItem(f *gatewaytest.Fake, id string) gateway.WorkItem {
	gw := gateway.New(f.URL(), "rs_test")
	it, err := gw.WorkItem(context.Background(), id)
	if err != nil || it == nil {
		panic("read item " + id)
	}
	return *it
}

// node.yaml's evaluate.heartbeat_every defaults to 5 minutes and is
// refused at or past the platform's 15-minute stall window.
func TestEvaluationHeartbeatInterval(t *testing.T) {
	if got := (&Config{}).EvaluationHeartbeatEvery(); got != 5*time.Minute {
		t.Errorf("default interval %s, want 5m", got)
	}
	if got := (&Config{Evaluate: EvaluateConfig{HeartbeatEvery: time.Minute}}).EvaluationHeartbeatEvery(); got != time.Minute {
		t.Errorf("interval %s, want node.yaml's 1m", got)
	}
	if err := (EvaluateConfig{HeartbeatEvery: 15 * time.Minute}).Validate(); err == nil || !strings.Contains(err.Error(), "evaluate.heartbeat_every") {
		t.Errorf("15m validates as %v, want refused naming evaluate.heartbeat_every", err)
	}
	if err := (EvaluateConfig{HeartbeatEvery: 14 * time.Minute}).Validate(); err != nil {
		t.Errorf("14m refused: %v", err)
	}
}
