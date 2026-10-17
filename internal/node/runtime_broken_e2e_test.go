package node

import (
	"bytes"
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
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// planGoalID is a second goal, waiting in planning, and item3 an item
// this node built that waits in evaluating: work of the other kinds a
// broken runtime must not claim either.
const (
	planGoalID = "77777777-7777-4777-8777-777777777777"
	item3      = "33333333-3333-4333-8333-333333333399"
)

// safeLog is a log the loop's goroutines and the test share.
type safeLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// claimsOfAnyKind counts every claim the node made of any kind.
func claimsOfAnyKind(f *gatewaytest.Fake) int {
	n := 0
	for _, name := range []string{"claimWorkItem", "claimWorkItemEvaluation", "claimPlan", "claimWorkItemRebase", "claimDirectionResearch", "claimMergeBatchValidation", "claimWorkItemMainValidation"} {
		n += len(f.CallsNamed(name))
	}
	return n
}

// runtimeNode is a sandboxed node on the fake gateway: its Loop, the
// Runner its Dispatch runs, the sandbox Fake under both, its run state
// and a clock the test moves.
type runtimeNode struct {
	f     *gatewaytest.Fake
	rt    *sandbox.Fake
	loop  *Loop
	state RunState
	log   *safeLog
	now   time.Time
	// mu guards the counters below, which the loop's goroutines bump
	mu         sync.Mutex
	dispatched []string
	evaluated  []string
	planned    []string
	runErrs    []error
}

func newRuntimeNode(t *testing.T, takesStatus bool) *runtimeNode {
	t.Helper()
	f := seed(t)
	if takesStatus {
		f.NodeStatuses = []string{RuntimeBrokenStatus}
	}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(2)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "n", Harnesses: []string{"claude-code"}, Capacity: 2, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	issueClaude(f, item1)
	issueClaude(f, item2)
	n := &runtimeNode{f: f, rt: &sandbox.Fake{}, log: &safeLog{}, state: RunState{Dir: t.TempDir()}, now: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)}
	logger := log.New(n.log, "", 0)
	sb := &sandbox.Sandbox{Runtime: n.rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	fakeAgent(n.rt, nil)
	ws := &fakeWS{root: t.TempDir()}
	declines := &Declines{}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: logger, Declines: declines,
		Now:            func() time.Time { return n.now },
		RuntimeProbe:   sb.Probe,
		RuntimeChanged: func(rt *RuntimeState) { _ = n.state.WriteRuntime(rt) },
		Evaluate: func(_ context.Context, item gateway.WorkItem) {
			n.mu.Lock()
			n.evaluated = append(n.evaluated, item.ID)
			n.mu.Unlock()
		},
		Plan: func(_ context.Context, g gateway.Goal, _ Scope) {
			n.mu.Lock()
			n.planned = append(n.planned, g.ID)
			n.mu.Unlock()
		},
	}
	r := &Runner{GW: gw, Harness: &harness.ClaudeCode{Sandbox: sb}, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour,
		Sandbox: sb, Declines: declines, RuntimeBroken: loop.NoteRuntimeBroken, Log: logger, Credentials: &Credentials{GW: gw, Log: logger}}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) {
		n.mu.Lock()
		n.dispatched = append(n.dispatched, item.ID)
		n.mu.Unlock()
		err := r.RunItem(ctx, item)
		n.mu.Lock()
		n.runErrs = append(n.runErrs, err)
		n.mu.Unlock()
	}
	n.loop = loop
	return n
}

// tick moves the clock by d and runs one tick to its end.
func (n *runtimeNode) tick(d time.Duration) {
	n.now = n.now.Add(d)
	n.loop.Tick(context.Background())
	n.loop.wg.Wait()
}

func (n *runtimeNode) breakRuntime() {
	n.rt.CreatePodErr = &sandbox.RuntimeError{Op: "pod create", Err: errors.New(podmanStorage)}
}

// moreWork seeds ready work of every kind beside the released item: a
// second ready item, a goal waiting to be planned and an item of this
// node's waiting in evaluating.
func (n *runtimeNode) moreWork() {
	f := n.f
	f.SetItem(item2, map[string]any{"status": "ready", "dependsOn": []string{}})
	f.Goals[planGoalID] = map[string]any{"id": planGoalID, "orgId": orgID, "projectId": projID, "slug": "plan-me", "title": "Plan me",
		"statement": "Plan.", "acceptance": "planned", "budgetCents": 1000, "spentCents": 0, "status": "planning"}
	f.WorkItems[item3] = map[string]any{"id": item3, "goalId": goalID, "orgId": orgID, "seq": 3, "title": "Built here",
		"brief": "b", "acceptance": "a", "repoId": repoID, "baseRef": "main", "status": "evaluating", "nodeId": n.loop.Cfg.NodeID, "dependsOn": []string{}}
}

// A node whose runtime breaks mid-run releases the item its pod could
// not start for as runtime_broken, then claims nothing — no item,
// evaluation or plan — over several ticks while ready work waits;
// heartbeats runtime_broken with podman's words; opens its holding
// summary with runtime_broken and names it on every ready goal; and
// leaves node.runtime. Once the runtime heals, the next probe a
// RuntimeProbeEvery later clears all of it and the node claims again.
func TestRuntimeBrokenStopsClaimingUntilAProbeHeals(t *testing.T) {
	if RuntimeProbeEvery > 10*time.Minute {
		t.Fatalf("RuntimeProbeEvery %s: the state must be seen within ten minutes", RuntimeProbeEvery)
	}
	n := newRuntimeNode(t, true)
	f, loop := n.f, n.loop

	// a healthy first tick probes, finds the runtime well, and claims
	// item1 — whose pod the runtime, broken since the probe, cannot
	// start
	n.breakRuntime()
	loop.RuntimeProbe = func(context.Context) error { return nil }
	n.tick(0)
	loop.RuntimeProbe = (&sandbox.Sandbox{Runtime: n.rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}).Probe
	if len(n.dispatched) != 1 || n.dispatched[0] != item1 {
		t.Fatalf("dispatched %v, want item1", n.dispatched)
	}
	rels := f.CallsNamed("releaseWorkItem")
	if len(rels) != 1 {
		t.Fatalf("releases = %+v", rels)
	}
	if reason, _ := rels[0].Input["note"].(string); !strings.HasPrefix(reason, "runtime_broken: ") || !strings.Contains(reason, "error opening storage") {
		t.Fatalf("release reason %q", reason)
	}
	if len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatal("the item was failed")
	}
	if it := f.Item(item1); it["status"] != "ready" {
		t.Fatalf("item1 %v, want ready", it["status"])
	}
	rt := loop.RuntimeBroken()
	if rt == nil || rt.Status != "runtime_broken" || !strings.Contains(rt.Reason, "error opening storage") {
		t.Fatalf("latch %+v", rt)
	}

	// ready work of every kind waits; tick after tick inside the probe
	// interval claims none of it
	n.moreWork()
	claims := claimsOfAnyKind(f)
	for range 3 {
		n.tick(time.Minute)
	}
	if got := claimsOfAnyKind(f); got != claims {
		t.Fatalf("%d claims made while the runtime is broken", got-claims)
	}
	if len(n.dispatched) != 1 || len(n.evaluated) != 0 || len(n.planned) != 0 {
		t.Fatalf("started while broken: dispatched %v evaluated %v planned %v", n.dispatched, n.evaluated, n.planned)
	}
	node := f.Nodes[loop.Cfg.NodeID]
	if node["status"] != "runtime_broken" || !strings.Contains(node["reason"].(string), "error opening storage") || strings.HasPrefix(node["reason"].(string), "runtime_broken: ") {
		t.Fatalf("heartbeat: status %v reason %v", node["status"], node["reason"])
	}
	h := loop.Holding()
	if h == nil || !strings.HasPrefix(h.Summary, "runtime_broken: ") || !strings.Contains(h.Summary, "error opening storage") {
		t.Fatalf("holding %+v", h)
	}
	if len(h.Unclaimed) == 0 {
		t.Fatal("no unclaimed rows while ready work waits")
	}
	for _, u := range h.Unclaimed {
		if !strings.HasPrefix(u.Reason, "runtime_broken: ") {
			t.Fatalf("unclaimed row %+v", u)
		}
	}
	saved, ok := n.state.Runtime()
	if !ok || saved.Status != "runtime_broken" || !strings.Contains(saved.Reason, "error opening storage") || saved.Since.IsZero() {
		t.Fatalf("node.runtime %+v %v", saved, ok)
	}

	// a probe on a still-broken runtime keeps the latch
	n.tick(RuntimeProbeEvery)
	if loop.RuntimeBroken() == nil || claimsOfAnyKind(f) != claims {
		t.Fatal("a failing probe let the node claim")
	}

	// healed: nothing changes before the next probe is due, then the
	// probe clears the latch, the file goes, and the node claims again
	n.rt.CreatePodErr = nil
	n.tick(time.Minute)
	if loop.RuntimeBroken() == nil || claimsOfAnyKind(f) != claims {
		t.Fatal("healed before a probe passed")
	}
	n.mu.Lock()
	n.dispatched = nil
	n.mu.Unlock()
	loop.Dispatch = func(_ context.Context, item gateway.WorkItem) {
		n.mu.Lock()
		n.dispatched = append(n.dispatched, item.ID)
		n.mu.Unlock()
	}
	n.tick(RuntimeProbeEvery)
	if rt := loop.RuntimeBroken(); rt != nil {
		t.Fatalf("still latched after a passing probe: %+v", rt)
	}
	if _, ok := n.state.Runtime(); ok {
		t.Fatal("node.runtime left after the heal")
	}
	if !strings.Contains(n.log.String(), "runtime healed") {
		t.Fatalf("no heal line:\n%s", n.log.String())
	}
	if claimsOfAnyKind(f) == claims || len(n.dispatched) == 0 {
		t.Fatalf("no claim after the heal (dispatched %v)", n.dispatched)
	}
	if len(f.CallsNamed("claimPlan")) == 0 || len(n.planned) == 0 {
		t.Fatal("the planning goal was not claimed after the heal")
	}
	if len(n.evaluated) == 0 {
		t.Fatal("the evaluating item was not evaluated after the heal")
	}
	if h := loop.Holding(); h == nil || strings.HasPrefix(h.Summary, "runtime_broken") {
		t.Fatalf("holding after the heal %+v", h)
	}
	// the next heartbeat is the ordinary one
	n.tick(time.Minute)
	if node := f.Nodes[loop.Cfg.NodeID]; node["status"] != "online" {
		t.Fatalf("status after the heal %v", node["status"])
	}
}

// A node started on a broken runtime is latched by its first tick's
// probe, before any session tried: it never claims.
func TestRuntimeBrokenAtStartNeverClaims(t *testing.T) {
	n := newRuntimeNode(t, true)
	n.breakRuntime()
	for range 3 {
		n.tick(time.Minute)
	}
	if got := claimsOfAnyKind(n.f); got != 0 || len(n.dispatched) != 0 {
		t.Fatalf("%d claims, dispatched %v", got, n.dispatched)
	}
	if n.f.Nodes[n.loop.Cfg.NodeID]["status"] != "runtime_broken" {
		t.Fatalf("status %v", n.f.Nodes[n.loop.Cfg.NodeID]["status"])
	}
	if !strings.Contains(n.log.String(), "runtime broken: ") {
		t.Fatalf("no latch line:\n%s", n.log.String())
	}
	n.rt.CreatePodErr = nil
	n.tick(RuntimeProbeEvery)
	if n.loop.RuntimeBroken() != nil || len(n.f.CallsNamed("claimWorkItem")) == 0 {
		t.Fatal("no claim once healed")
	}
}

// A gateway that does not take runtime_broken as a status gets none:
// the heartbeat stays online and the words ride its reason, prefixed,
// as the sandbox's limit condition does (ADR-034).
func TestRuntimeBrokenOnAGatewayWithoutTheStatus(t *testing.T) {
	n := newRuntimeNode(t, false)
	n.breakRuntime()
	n.tick(0)
	node := n.f.Nodes[n.loop.Cfg.NodeID]
	reason, _ := node["reason"].(string)
	if node["status"] != "online" || !strings.HasPrefix(reason, "runtime_broken: ") || !strings.Contains(reason, "error opening storage") {
		t.Fatalf("heartbeat: status %v reason %q", node["status"], reason)
	}
	if got := claimsOfAnyKind(n.f); got != 0 {
		t.Fatalf("%d claims", got)
	}
}
