package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// requestRebase puts the item in review with its rebase requested, as
// the integrator's RequestWorkItemRebase leaves it (runsheet ADR 008).
func requestRebase(f *gatewaytest.Fake, id, branch string) {
	row := f.WorkItems[id]
	row["status"], row["branch"] = "review", branch
	row["rebaseState"], row["rebaseConflict"], row["rebaseFiles"] = "requested", "<<<<<<< HEAD\nfunc Run() {}\n=======\nfunc Run(ctx context.Context) {}\n>>>>>>> main", []any{"main.go"}
}

// rebaseNode is a node over the seeded org that rebases with h and ws.
func rebaseNode(t *testing.T, f *gatewaytest.Fake, slug string, h harness.Harness, ws *fakeWS, logs *strings.Builder) (*Loop, *Config) {
	t.Helper()
	// the rebase holder's token: the platform half ADR-001 asks for
	// (TestRebaseTokenRefusedFailsTheRebase is the platform as it is)
	f.RebaseTokens = true
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: slug, NodeID: gateway.NodeID(mustUUID(orgID), slug).String(), Capacity: Fixed(1)}
	if err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: slug, Slug: slug}); err != nil {
		t.Fatal(err)
	}
	var w = log.New(os.Stderr, slug+" ", 0)
	if logs != nil {
		w = log.New(logs, "", 0)
	}
	rb := &Rebaser{GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MCP: PrologueMCP(f.URL(), "rs_test"), MaxBudgetUSD: 4, ProgressEvery: time.Hour, Log: w}
	return &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: w,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Rebase:   func(ctx context.Context, item gateway.WorkItem) { _ = rb.Rebase(ctx, item) }}, cfg
}

// A requested rebase is claimed once, runs one session of kind rebase in
// the item's own checkout on its own token, and finishes with the head
// origin has — read back, not taken on the session's word — and the
// files the session resolved. Its cost goes on the rebase's heartbeat.
func TestRebaseClaimedOnceAndFinishedWithPushedHead(t *testing.T) {
	f := seed(t)
	requestRebase(f, item1, "mesh/ship-it-1")
	ws := &fakeWS{root: t.TempDir(), pushedHead: "abc123def4567890"}
	h := &fakeHarness{text: "main.go: kept the base's ctx and the branch's retry.\n\nREBASED: abc123def456\nRESOLVED: main.go"}
	loop, cfg := rebaseNode(t, f, "test-node", h, ws, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()
	loop.Tick(ctx) // nothing left requested
	loop.wg.Wait()

	claims := f.CallsNamed("claimWorkItemRebase")
	if len(claims) != 1 || claims[0].Input["aggregateId"] != item1 || claims[0].Input["nodeId"] != cfg.NodeID {
		t.Fatalf("claims: %+v", claims)
	}
	if len(h.jobs) != 1 || h.jobs[0].Kind != harness.KindRebase || h.jobs[0].MaxBudgetUSD != 4 || h.jobs[0].MCPServers["prologue"] == nil {
		t.Fatalf("jobs: %+v", h.jobs)
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{"git rebase origin/main", "git push --force-with-lease origin mesh/ship-it-1", "func Run(ctx context.Context) {}", "The files: main.go.", harness.RebasedLine, harness.RebaseFailedLine} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if len(ws.prepared) != 1 || ws.prepared[0].Branch != "mesh/ship-it-1" || ws.prepared[0].BaseRef != "main" || ws.prepared[0].Dir() != (WorkspaceSpec{Root: ws.root, GoalSlug: "ship-it", Seq: 1}).Dir() {
		t.Fatalf("prepared: %+v", ws.prepared)
	}
	if ev := ws.Events(); strings.Join(ev, ",") != "authorize ghs_item1,prepare,pushed-head mesh/ship-it-1" {
		t.Fatalf("workspace events: %v", ev)
	}
	if len(ws.pushed) != 0 {
		t.Fatal("the session pushes, not the node")
	}
	finishes := f.CallsNamed("finishWorkItemRebase")
	if len(finishes) != 1 || finishes[0].Input["headSha"] != "abc123def4567890" || finishes[0].Input["method"] != "session" ||
		!reflect.DeepEqual(finishes[0].Input["files"], []any{"main.go"}) {
		t.Fatalf("finishes: %+v", finishes)
	}
	if len(f.CallsNamed("failWorkItemRebase")) != 0 {
		t.Fatal("a rebase that landed is not failed")
	}
	it := f.Item(item1)
	if it["rebaseState"] != "rebased" || it["status"] != "review" || it["costCents"] != 125 || it["attempts"] != nil {
		t.Fatalf("item: %v", it)
	}
}

// A session that cannot resolve the conflict fails the rebase with its
// reason, which hands the item to its builder; so does one whose output
// cannot be read, and one whose push did not land — none holds the claim.
func TestRebaseUnresolvableFailsWithReason(t *testing.T) {
	const why = "main.go Run: the base removed the retry loop the branch's change is inside"
	for _, c := range []struct {
		name, text, pushed string
		pushedErr          error
		reason             string
	}{
		{"unresolvable", "Both sides rewrote Run.\nREBASE FAILED: " + why, "", nil, why},
		{"unreadable", "I think it is fine now.", "", nil, "the rebase session reached no result (s-1): I think it is fine now."},
		{"push did not land", "REBASED: 1111111aaaa\nRESOLVED: main.go", "2222222bbbb", nil, "the rebase session said it pushed 1111111a, but origin/mesh/ship-it-1 is at 2222222b: the push did not land"},
		{"not onto the base", "REBASED: 1111111aaaa\nRESOLVED: main.go", "", errors.New("origin/mesh/ship-it-1 at 1111111a does not contain origin/main"), "the rebase session said it pushed 1111111a, but: origin/mesh/ship-it-1 at 1111111a does not contain origin/main"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := seed(t)
			requestRebase(f, item1, "mesh/ship-it-1")
			ws := &fakeWS{root: t.TempDir(), pushedHead: c.pushed, pushedErr: c.pushedErr}
			h := &fakeHarness{text: c.text}
			loop, _ := rebaseNode(t, f, "test-node", h, ws, nil)
			loop.Tick(context.Background())
			loop.wg.Wait()

			fails := f.CallsNamed("failWorkItemRebase")
			if len(fails) != 1 || fails[0].Input["aggregateId"] != item1 || fails[0].Input["reason"] != c.reason {
				t.Fatalf("fails: %+v", fails)
			}
			if len(f.CallsNamed("finishWorkItemRebase")) != 0 {
				t.Fatal("a failed rebase is not finished")
			}
			// the builder has it now, with the reason, attempts unchanged
			if it := f.Item(item1); it["status"] != "ready" || it["lastError"] != c.reason || it["rebaseState"] != "none" || it["costCents"] != 125 {
				t.Fatalf("item: %v", it)
			}
		})
	}
}

// The platform as it is (runsheet gateway/itemtoken.go) gives an item's
// token to no one while the item is in review, so the rebase holder is
// refused it: that fails the rebase with the reason — the item goes to
// its builder — rather than holding the claim or pushing on the
// operator's own login, and no session starts.
func TestRebaseTokenRefusedFailsTheRebase(t *testing.T) {
	f := seed(t)
	requestRebase(f, item1, "mesh/ship-it-1")
	h := &fakeHarness{text: "REBASED: abc123def456\nRESOLVED: main.go"}
	ws := &fakeWS{root: t.TempDir(), pushedHead: "abc123def456"}
	loop, _ := rebaseNode(t, f, "test-node", h, ws, nil)
	f.RebaseTokens = false
	loop.Tick(context.Background())
	loop.wg.Wait()

	fails := f.CallsNamed("failWorkItemRebase")
	if len(fails) != 1 || !strings.HasPrefix(fmt.Sprint(fails[0].Input["reason"]), "github token: ") || !strings.Contains(fmt.Sprint(fails[0].Input["reason"]), "item is review, not claimed") {
		t.Fatalf("fails: %+v", fails)
	}
	if len(h.jobs) != 0 || len(ws.Events()) != 0 || len(f.CallsNamed("finishWorkItemRebase")) != 0 {
		t.Fatalf("no session, no checkout: jobs %d, workspace %v", len(h.jobs), ws.Events())
	}
	if it := f.Item(item1); it["status"] != "ready" || it["rebaseState"] != "none" {
		t.Fatalf("item: %v", it)
	}
}

// A rebase that landed but whose finish the platform did not take is
// failed with the reason, so the claim is never left held.
func TestRebaseFinishRefusedFailsTheRebase(t *testing.T) {
	f := seed(t)
	requestRebase(f, item1, "mesh/ship-it-1")
	f.Refuse["finishWorkItemRebase"] = "finishWorkItemRebase: the head is not on the branch"
	h := &fakeHarness{text: "REBASED: abc123def456\nRESOLVED: main.go"}
	loop, _ := rebaseNode(t, f, "test-node", h, &fakeWS{root: t.TempDir(), pushedHead: "abc123def4567890"}, nil)
	loop.Tick(context.Background())
	loop.wg.Wait()

	fails := f.CallsNamed("failWorkItemRebase")
	if len(fails) != 1 || !strings.HasPrefix(fmt.Sprint(fails[0].Input["reason"]), "the rebase landed at abc123de, but finishing it failed: ") ||
		!strings.Contains(fmt.Sprint(fails[0].Input["reason"]), "the head is not on the branch") {
		t.Fatalf("fails: %+v", fails)
	}
	if it := f.Item(item1); it["rebaseState"] != "none" || it["status"] != "ready" {
		t.Fatalf("item: %v", it)
	}
}

// staleRebases lists what was requested before another node claimed it:
// the race two nodes run on one platform.
type staleRebases struct {
	Gateway
	rows []gateway.WorkItem
	// refused collects the claims the platform said no to.
	refused *[]error
}

func (s staleRebases) ClaimWorkItemRebase(ctx context.Context, itemID, nodeID string) error {
	err := s.Gateway.ClaimWorkItemRebase(ctx, itemID, nodeID)
	if err != nil && s.refused != nil {
		*s.refused = append(*s.refused, err)
	}
	return err
}

func (s staleRebases) RequestedRebases(context.Context, string) ([]gateway.WorkItem, error) {
	return s.rows, nil
}

// Two nodes that both see one requested rebase never both claim it: the
// second is refused, runs no session, and does not log the refusal as
// an error.
func TestRebaseTwoNodesOneClaim(t *testing.T) {
	f := seed(t)
	requestRebase(f, item1, "mesh/ship-it-1")
	gw := gateway.New(f.URL(), "rs_test")
	seen, err := gw.RequestedRebases(context.Background(), orgID)
	if err != nil || len(seen) != 1 {
		t.Fatalf("requested: %v %v", seen, err)
	}
	release := make(chan struct{})
	hA := &blockingHarness{fakeHarness: fakeHarness{text: "REBASED: abc123def456\nRESOLVED: main.go"}, release: release}
	hB := &fakeHarness{text: "REBASED: abc123def456\nRESOLVED: main.go"}
	a, cfgA := rebaseNode(t, f, "node-a", hA, &fakeWS{root: t.TempDir(), pushedHead: "abc123def456"}, nil)
	var logsB strings.Builder
	b, _ := rebaseNode(t, f, "node-b", hB, &fakeWS{root: t.TempDir(), pushedHead: "abc123def456"}, &logsB)
	var refused []error
	b.GW = staleRebases{Gateway: b.GW, rows: seen, refused: &refused}

	ctx := context.Background()
	a.Tick(ctx)
	waitFor(t, func() bool { return hA.started() == 1 })
	b.Tick(ctx) // B still sees it requested and asks for it
	b.wg.Wait()
	close(release)
	a.wg.Wait()

	claims := f.CallsNamed("claimWorkItemRebase")
	if len(claims) != 1 || claims[0].Input["nodeId"] != cfgA.NodeID || len(refused) != 1 || !gateway.IsRefusal(refused[0]) {
		t.Fatalf("both nodes ask, one is given it: %+v, refused %v", claims, refused)
	}
	if it := f.Item(item1); it["rebaseNodeId"] != cfgA.NodeID || it["rebaseState"] != "rebased" {
		t.Fatalf("item: %v", it)
	}
	if hA.started() != 1 || len(hB.jobs) != 0 {
		t.Fatalf("sessions: a %d, b %d", hA.started(), len(hB.jobs))
	}
	if len(f.CallsNamed("finishWorkItemRebase")) != 1 {
		t.Fatal("one finish")
	}
	if strings.Contains(logsB.String(), "claim") {
		t.Fatalf("a refusal is the ordinary answer, not an error:\n%s", logsB.String())
	}
}

// Rebases are claimed most urgent goal first across the orgs, then by
// seq, as the rest of the node's work is.
func TestRebaseClaimsUrgentBeforeLow(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "chores", "low")
	p.addGoal(org2, "fire", "urgent")
	p.addGoal(orgID, "steady", "")
	for _, it := range []struct {
		org, goal, id string
		seq           int
	}{
		{orgID, "chores", "low-1", 1}, {orgID, "steady", "normal-4", 4}, {org2, "fire", "urgent-9", 9}, {orgID, "steady", "normal-2", 2},
	} {
		p.addItem(it.org, it.goal, it.id, it.seq, false)
		p.WorkItems[it.id]["status"], p.WorkItems[it.id]["rebaseState"] = "review", "requested"
	}
	loop, _ := p.loop(1)
	var mu sync.Mutex
	var ran []string
	loop.Rebase = func(_ context.Context, item gateway.WorkItem) {
		mu.Lock()
		ran = append(ran, item.ID)
		mu.Unlock()
	}
	loop.Tick(context.Background())
	loop.wg.Wait()
	var claimed []string
	for _, c := range p.CallsNamed("claimWorkItemRebase") {
		claimed = append(claimed, fmt.Sprint(c.Input["aggregateId"]))
	}
	if got := strings.Join(claimed, ","); got != "urgent-9,normal-2,normal-4,low-1" {
		t.Fatalf("claimed %s", got)
	}
	if len(ran) != 4 {
		t.Fatalf("every claim runs a session: %v", ran)
	}
	if p.CallsNamed("claimWorkItemRebase")[0].Input["nodeId"] != p.scopes[1].NodeID {
		t.Fatal("globex's rebase is claimed as the node in globex")
	}
}

// A rebase in flight is not a build: it takes no capacity, so a node at
// capacity 1 still claims a ready item beside it, heartbeats running 1,
// and a second tick does not start the rebase again.
func TestRebaseDoesNotConsumeCapacity(t *testing.T) {
	f := seed(t)
	requestRebase(f, item2, "mesh/ship-it-2")
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	if err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node"}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var rebases, builds sync.WaitGroup
	rebases.Add(1)
	builds.Add(1)
	var rebaseRuns, buildRuns int
	var mu sync.Mutex
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour,
		Rebase: func(ctx context.Context, item gateway.WorkItem) {
			mu.Lock()
			rebaseRuns++
			mu.Unlock()
			rebases.Done()
			<-release
		},
		Dispatch: func(ctx context.Context, item gateway.WorkItem) {
			mu.Lock()
			buildRuns++
			mu.Unlock()
			builds.Done()
			<-release
		}}
	ctx := context.Background()
	loop.Tick(ctx)
	rebases.Wait()
	builds.Wait()
	if loop.Running() != 1 || !reflect.DeepEqual(loop.RunningIDs(), []string{item1}) {
		t.Fatalf("running: %v", loop.RunningIDs())
	}
	// the claim is still held (the fake lists it no more), and the
	// in-flight map keeps a stale listing from starting it twice
	loop.GW = staleRebases{Gateway: gw, rows: []gateway.WorkItem{{ID: item2, GoalID: goalID, OrgID: orgID, Seq: 2}}}
	loop.Tick(ctx)
	hb := f.CallsNamed("heartbeatNode")
	if last := hb[len(hb)-1]; last.Input["running"] != float64(1) {
		t.Fatalf("heartbeat: %+v", last.Input)
	}
	close(release)
	loop.wg.Wait()
	if rebaseRuns != 1 || buildRuns != 1 || len(f.CallsNamed("claimWorkItemRebase")) != 1 || len(f.CallsNamed("claimWorkItem")) != 1 {
		t.Fatalf("rebases %d, builds %d, rebase claims %d, claims %d", rebaseRuns, buildRuns, len(f.CallsNamed("claimWorkItemRebase")), len(f.CallsNamed("claimWorkItem")))
	}
}

// A draining node claims no rebase, and waits for one in flight.
func TestRebaseDrain(t *testing.T) {
	f := seed(t)
	requestRebase(f, item1, "mesh/ship-it-1")
	loop, _ := rebaseNode(t, f, "test-node", &fakeHarness{}, &fakeWS{root: t.TempDir()}, nil)
	loop.Drain()
	loop.Tick(context.Background())
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimWorkItemRebase")); n != 0 {
		t.Fatalf("a draining node claimed %d rebase(s)", n)
	}
	loop.mu.Lock()
	loop.rebasing = map[string]bool{item1: true}
	loop.mu.Unlock()
	if loop.drained() {
		t.Fatal("a rebase in flight keeps the node from having drained")
	}
}

// blockingHarness is fakeHarness held at the start of its run until
// release is closed.
type blockingHarness struct {
	fakeHarness
	release chan struct{}
	mu      sync.Mutex
	n       int
}

func (h *blockingHarness) started() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

func (h *blockingHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	<-h.release
	return h.fakeHarness.Run(ctx, job, on)
}
