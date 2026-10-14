package node

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// An item's evaluation runs on one node (goal
// an-items-evaluation-runs-on-one-node, ADR-025's amendment 2026-10-14):
// two loops on one fake gateway — node A, which built item 1 and holds
// it in evaluating, and node B, which takes evaluations only — tick over
// the same item. The platform's claimWorkItemEvaluation is the one lock,
// the builder's own evaluation included, so across both node logs there
// is exactly one `evaluate on` line for the item, no `not yours`, and one
// evaluateWorkItem recorded, whichever node ticks first.

// gatedHarness is a fakeHarness whose sessions wait for open before
// they answer, so a test can tick the other node while one evaluation
// is still running.
type gatedHarness struct {
	*fakeHarness
	open chan struct{}
}

func (h *gatedHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	select {
	case <-h.open:
	case <-ctx.Done():
		return harness.Result{}, ctx.Err()
	}
	return h.fakeHarness.Run(ctx, job, on)
}

// oneNodeNode is one of the two nodes: its loop, its log and the
// evaluator sessions its harness ran.
type oneNodeNode struct {
	name string
	cfg  *Config
	loop *Loop
	logs *syncBuffer
	h    *gatedHarness
}

// syncBuffer is a log sink both a loop and its evaluation goroutines
// write to.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// oneNodeFixture is the fake gateway with item 1 in evaluating, built
// and held by node A, and the two nodes: A takes every kind, B
// evaluations only. Each evaluates with a real Evaluator on its own
// workspace and a harness that passes the item once open is closed.
func oneNodeFixture(t *testing.T, open chan struct{}) (*gatewaytest.Fake, *oneNodeNode, *oneNodeNode) {
	t.Helper()
	f := seed(t)
	mk := func(name string, takes Takes) *oneNodeNode {
		cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: name,
			NodeID: gateway.NodeID(mustUUID(orgID), name).String(), Capacity: Fixed(2), Takes: takes}
		gw := gateway.New(f.URL(), "rs_test")
		if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID,
			Name: name, Slug: name, Harnesses: []string{"claude-code"}, Capacity: 2, Version: "test"}); err != nil {
			t.Fatal(err)
		}
		logs := &syncBuffer{}
		logger := log.New(logs, name+": ", 0)
		h := &gatedHarness{fakeHarness: &fakeHarness{text: "The test fails as asked.\n\nVERDICT: PASS"}, open: open}
		ws := &fakeWS{root: t.TempDir()}
		ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg),
			WorkRoot: ws.root, MaxBudgetUSD: 2, Log: logger}
		loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Harness: "claude-code", Log: logger,
			Dispatch: func(context.Context, gateway.WorkItem) {},
			Plan:     func(context.Context, gateway.Goal, Scope) {},
			Research: func(context.Context, gateway.Direction, gateway.Project, Scope) {},
		}
		// as nodecmd's evaluateWith: an error is the loop's to log
		loop.Evaluate = func(ctx context.Context, item gateway.WorkItem) {
			if err := ev.Evaluate(ctx, item); err != nil && ctx.Err() == nil {
				logger.Printf("item %d %q: evaluation: %v", item.Seq, item.Title, err)
			}
		}
		return &oneNodeNode{name: name, cfg: cfg, loop: loop, logs: logs, h: h}
	}
	a := mk("node-a", nil)
	b := mk("node-b", Takes{{Kind: "evaluate"}})
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = a.cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	return f, a, b
}

// tick runs one tick of n's loop, leaving its evaluations running.
func (n *oneNodeNode) tick() { n.loop.Tick(context.Background()) }

// wait waits for n's evaluations to end.
func (n *oneNodeNode) wait() { n.loop.wg.Wait() }

// assertOneEvaluation checks both logs and the gateway: one `evaluate
// on` for item 1, no `not yours`, one evaluateWorkItem, the item out of
// evaluating, and the session run by want alone.
func assertOneEvaluation(t *testing.T, f *gatewaytest.Fake, want *oneNodeNode, nodes ...*oneNodeNode) {
	t.Helper()
	var all strings.Builder
	for _, n := range nodes {
		all.WriteString(n.logs.String())
	}
	logs := all.String()
	if n := strings.Count(logs, "item ship-it/1: evaluate on "); n != 1 {
		t.Errorf("%d `evaluate on` lines for item 1 across both logs, want 1:\n%s", n, logs)
	}
	if strings.Contains(logs, "not yours") {
		t.Errorf("a `not yours` line in the logs:\n%s", logs)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 1 {
		t.Errorf("%d evaluateWorkItem call(s), want 1:\n%s", len(calls), logs)
	} else if calls[0].Input["passed"] != true {
		t.Errorf("evaluateWorkItem input = %v, want passed", calls[0].Input)
	}
	if row := f.Item(item1); row["status"] == "evaluating" {
		t.Errorf("item 1 is still evaluating: %v", row)
	}
	if row := f.Item(item1); row["evaluationNodeId"] != want.cfg.NodeID {
		t.Errorf("evaluationNodeId = %v, want %s's %s", row["evaluationNodeId"], want.name, want.cfg.NodeID)
	}
	for _, n := range nodes {
		n.h.mu.Lock()
		ran := len(n.h.jobs)
		n.h.mu.Unlock()
		if n == want && ran != 1 {
			t.Errorf("%s ran %d evaluation session(s), want 1", n.name, ran)
		}
		if n != want && ran != 0 {
			t.Errorf("%s ran %d evaluation session(s), want none", n.name, ran)
		}
	}
}

// A ticks first: the builder claims its own evaluation (the platform
// converges for it) and evaluates; B's claim, ticked while A's session
// runs, is refused — the ordinary answer, said nowhere — and B starts
// nothing.
func TestEvaluateOneNodeBuilderTicksFirst(t *testing.T) {
	open := make(chan struct{})
	f, a, b := oneNodeFixture(t, open)
	a.tick()
	b.tick()
	b.tick()
	close(open)
	a.wait()
	b.wait()
	for range 3 {
		a.tick()
		b.tick()
		a.wait()
		b.wait()
	}
	assertOneEvaluation(t, f, a, a, b)
	if out := b.logs.String(); strings.Contains(out, "evaluating item") || strings.Contains(out, "claim the evaluation") {
		t.Errorf("node B started or erred on A's evaluation:\n%s", out)
	}
	byNode := map[any]int{}
	for _, c := range f.Claims {
		if c.Name == "claimWorkItemEvaluation" {
			byNode[c.Input["nodeId"]]++
		}
	}
	if byNode[a.cfg.NodeID] == 0 || byNode[b.cfg.NodeID] == 0 {
		t.Errorf("claimWorkItemEvaluation by node = %v, want A's and B's", byNode)
	}
}

// B ticks first: the evaluate-only node claims the evaluation and runs
// it; A, the builder, ticked while it runs, starts no session. The claim
// made the item B's (its nodeId names the evaluator now), so A's own
// claim is refused, or never made, as for any other node's item; were
// it to see the item as its own and be refused, it says so once — `not
// evaluated here`, never `not yours`.
func TestEvaluateOneNodeEvaluatorTicksFirst(t *testing.T) {
	open := make(chan struct{})
	f, a, b := oneNodeFixture(t, open)
	b.tick()
	a.tick()
	a.tick()
	close(open)
	a.wait()
	b.wait()
	for range 3 {
		b.tick()
		a.tick()
		a.wait()
		b.wait()
	}
	assertOneEvaluation(t, f, b, a, b)
	out := a.logs.String()
	if strings.Contains(out, "evaluating item") || strings.Contains(out, "claim the evaluation") {
		t.Errorf("node A started or erred on B's evaluation:\n%s", out)
	}
	if n := strings.Count(out, "not evaluated here"); n > 1 {
		t.Errorf("node A said `not evaluated here` %d times, want at most once:\n%s", n, out)
	}
}

// The builder's view is stale — it listed the item as its own before B
// claimed it — and its own claim is refused: it says once that the
// evaluation is B's, `not evaluated here`, and starts nothing.
func TestEvaluateOneNodeBuilderRefusedSaysNotEvaluatedHere(t *testing.T) {
	open := make(chan struct{})
	f, a, b := oneNodeFixture(t, open)
	// B's claim lands while A still reads the item as its own: the
	// evaluation holder is B, the row's nodeId still A
	if err := b.loop.GW.ClaimWorkItemEvaluation(context.Background(), item1, b.cfg.NodeID); err != nil {
		t.Fatal(err)
	}
	f.WorkItems[item1]["nodeId"] = a.cfg.NodeID
	a.tick()
	a.tick()
	close(open)
	a.wait()
	out := a.logs.String()
	want := fmt.Sprintf("item ship-it/1: evaluation is node %s's; not evaluated here", b.cfg.NodeID)
	if n := strings.Count(out, want); n != 1 {
		t.Errorf("node A logged %q %d times, want once:\n%s", want, n, out)
	}
	if strings.Contains(out, "evaluate on") || strings.Contains(out, "not yours") {
		t.Errorf("node A evaluated, or said `not yours`:\n%s", out)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 0 {
		t.Errorf("%d evaluateWorkItem call(s) from the refused builder, want none", len(calls))
	}
}

// Both tick at once, several rounds: whichever claim the platform takes
// first evaluates, and the other starts nothing.
func TestEvaluateOneNodeConcurrentTicks(t *testing.T) {
	for round := range 10 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			open := make(chan struct{})
			f, a, b := oneNodeFixture(t, open)
			var wg sync.WaitGroup
			for range 3 {
				wg.Add(2)
				go func() { defer wg.Done(); a.tick() }()
				go func() { defer wg.Done(); b.tick() }()
				wg.Wait()
			}
			close(open)
			for range 2 {
				a.wait()
				b.wait()
				wg.Add(2)
				go func() { defer wg.Done(); a.tick() }()
				go func() { defer wg.Done(); b.tick() }()
				wg.Wait()
			}
			a.wait()
			b.wait()
			holder, _ := f.Item(item1)["evaluationNodeId"].(string)
			winner := a
			if holder == b.cfg.NodeID {
				winner = b
			}
			assertOneEvaluation(t, f, winner, a, b)
		})
	}
}
