package node

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// planningGoal2 is a second goal of seed's project, in planning.
const planningGoal2 = "dddddddd-3333-4333-8333-dddddddddddd"

// fresh is a reading with all the headroom in the world.
func fresh(at time.Time) *harness.RateLimit {
	return &harness.RateLimit{Status: "allowed", ResetsAt: at.Add(5 * time.Hour),
		FiveHour: harness.RateLimitWindow{Utilization: 0.1, ResetsAt: at.Add(5 * time.Hour)},
		SevenDay: harness.RateLimitWindow{Utilization: 0.1, ResetsAt: at.Add(72 * time.Hour)}}
}

// headroomRig is a node on seed's fake whose own chooser has headroom —
// it knows a fresh subscription, max-other — while the one credential
// the claim-time read issues the project, max-issued, is exhausted
// until T: the claim goes through and the read refuses it.
type headroomRig struct {
	f      *gatewaytest.Fake
	gw     *gateway.Client
	loop   *Loop
	h      *fakeHarness
	pl     *Planner
	store  *NoCredentials
	limits *Limits
	logs   *syncBuf
	now    *time.Time
	T      time.Time
}

func newHeadroomRig(t *testing.T, capacity int, credsGW func(*gateway.Client) CredentialGateway) *headroomRig {
	t.Helper()
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.WorkItems[item2]["status"], f.WorkItems[item2]["dependsOn"] = "ready", []string{}
	f.Goals[planningGoal2] = map[string]any{"id": planningGoal2, "orgId": orgID, "projectId": projID, "slug": "new-thing", "title": "New thing",
		"statement": "Build the new thing.", "acceptance": "the thing works", "budgetCents": 2000, "spentCents": 0, "status": "planning"}
	f.DefaultSessionCredentials = []map[string]any{claudeCredential} // max-issued
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(capacity), OwnerID: f.UserID}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: capacity, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	r := &headroomRig{f: f, gw: gw, h: &fakeHarness{}, logs: &syncBuf{}, now: &now, T: now.Add(90 * time.Minute), limits: &Limits{}}
	clock := func() time.Time { return *r.now }
	logger := log.New(r.logs, "", 0)
	r.store = &NoCredentials{Log: logger, Now: clock}
	known := &KnownAccounts{}
	for _, a := range []AccountConfig{{Label: "max-other", Kind: AccountSubscription, ID: "cred-other"}, {Label: "max-issued", Kind: AccountSubscription, ID: "cred-claude"}} {
		if err := known.Note(a, now); err != nil {
			t.Fatal(err)
		}
	}
	r.limits.Observe("max-other", fresh(now))
	r.limits.Observe("max-issued", exhausted(r.T))
	var cg CredentialGateway = gw
	if credsGW != nil {
		cg = credsGW(gw)
	}
	creds := &Credentials{GW: cg, Limits: r.limits, NoCredential: r.store, Now: clock, Log: logger}
	ws := &fakeWS{root: t.TempDir()}
	runner := &Runner{GW: gw, Harness: r.h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour, Log: logger,
		MCP: MeshContextHTTP(f.URL(), "rs_test"), Credentials: creds}
	r.pl = &Planner{Credentials: creds, GW: gw, Harness: r.h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: logger}
	r.loop = &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: logger,
		NoCredential: r.store, Known: known, Limits: r.limits, Now: clock,
		Dispatch: func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) },
		Plan:     func(ctx context.Context, g gateway.Goal, sc Scope) { _ = r.pl.Plan(ctx, g, sc) }}
	return r
}

// buildReason is the hold's reason for seed's project's builds, refused
// on max-issued until T.
func (r *headroomRig) reason(kind string) string {
	return "no issued credential has headroom for " + kind + " on org acme's project mesh (max-issued exhausted until " + r.T.Format(time.RFC3339) + ")"
}

// The node's chooser has headroom, the issued credential none: the
// first tick claims one item and the goal in planning, both reads
// refuse, the item goes back to ready with its attempts as they were
// and no session starts; the second tick within the hold claims
// neither, and node.log names each goal's reason once.
func TestNoHeadroomLoopReleasesAndHoldsItemsAndPlans(t *testing.T) {
	r := newHeadroomRig(t, 1, nil)
	attempts := r.f.Item(item1)["attempts"]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r.loop.Tick(ctx)
	waitFor(t, func() bool { return len(r.f.CallsNamed("releaseWorkItem")) == 1 && r.loop.Running() == 0 })
	r.loop.wg.Wait()
	if n := len(r.f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("first tick claimed %d item(s), want 1", n)
	}
	if n := len(r.f.CallsNamed("claimPlan")); n != 1 {
		t.Fatalf("first tick claimed %d plan(s), want 1", n)
	}
	rel := r.f.CallsNamed("releaseWorkItem")[0].Input
	if rel["note"] != "accounts: "+r.reason("build") {
		t.Fatalf("release note = %q", rel["note"])
	}
	it := r.f.Item(rel["aggregateId"].(string))
	if it["status"] != "ready" || it["attempts"] != attempts {
		t.Fatalf("released item = status %v attempts %v, want ready and %v", it["status"], it["attempts"], attempts)
	}
	if len(r.h.jobs) != 0 {
		t.Fatalf("%d sessions started", len(r.h.jobs))
	}

	*r.now = r.now.Add(10 * time.Minute) // within the hold
	claims, plans := len(r.f.CallsNamed("claimWorkItem")), len(r.f.CallsNamed("claimPlan"))
	r.loop.Tick(ctx)
	r.loop.wg.Wait()
	r.loop.Tick(ctx)
	r.loop.wg.Wait()
	if n := len(r.f.CallsNamed("claimWorkItem")) - claims; n != 0 {
		t.Fatalf("claimed %d item(s) within the hold", n)
	}
	if n := len(r.f.CallsNamed("claimPlan")) - plans; n != 0 {
		t.Fatalf("claimed %d plan(s) within the hold", n)
	}
	logs := r.logs.String()
	for _, kind := range []string{"build", "plan"} {
		if n := strings.Count(logs, r.reason(kind)); n != 1 {
			t.Fatalf("node.log says the %s reason %d times, want once:\n%s", kind, n, logs)
		}
	}
	if n := strings.Count(logs, "goal "+goalID+"'s work left unclaimed"); n != 1 {
		t.Fatalf("goal said %d times:\n%s", n, logs)
	}
	// the plan's refusal is a hold, not a try
	r.pl.mu.Lock()
	tries := r.pl.attempts[planningGoal2]
	r.pl.mu.Unlock()
	if tries != 0 || strings.Contains(logs, "no plan after") {
		t.Fatalf("plan attempts = %d after a refusal, want 0:\n%s", tries, logs)
	}
	h := r.loop.Holding()
	if h == nil {
		t.Fatal("no holding report")
	}
	got := map[string]string{}
	for _, u := range h.Unclaimed {
		got[u.GoalID+"/"+u.Kind] = u.Reason
	}
	if !strings.Contains(got[goalID+"/item"], r.reason("build")) || !strings.Contains(got[planningGoal2+"/plan"], r.reason("plan")) {
		t.Fatalf("holding.unclaimed = %+v", h.Unclaimed)
	}
}

// gatedCreds holds every claim-time read until gate closes: the probe's
// read stays unresolved while the test ticks.
type gatedCreds struct {
	*gateway.Client
	gate  chan struct{}
	reads atomic.Int32
}

func (g *gatedCreds) Session(ctx context.Context, ref gateway.SessionRef, nodeID string) (*gateway.Session, error) {
	g.reads.Add(1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.Client.Session(ctx, ref, nodeID)
}

// At the hold's until — the refused credential's resets_at — one claim
// goes through for the project's builds, the other ready item held; a
// tick while that probe's read is unresolved claims nothing; the reading
// observed fresh before T's tick, the probe's item runs.
func TestNoHeadroomHoldLiftsAtResetsAtWithOneProbe(t *testing.T) {
	var gated *gatedCreds
	r := newHeadroomRig(t, 2, func(gw *gateway.Client) CredentialGateway {
		gated = &gatedCreds{Client: gw, gate: make(chan struct{})}
		return gated
	})
	r.loop.Plan = nil
	r.store.NoteHeadroom(goalID, &ErrNoHeadroom{Org: "acme", Project: "mesh", ProjectID: projID, Kind: string(harness.KindBuild), Adapter: AdapterClaudeCode, Refused: []string{"max-issued"}, Until: r.T})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	*r.now = r.T.Add(-time.Minute)
	r.loop.Tick(ctx)
	if n := len(r.f.CallsNamed("claimWorkItem")); n != 0 {
		t.Fatalf("claimed %d before T", n)
	}

	*r.now = r.T
	r.limits.Observe("max-issued", fresh(r.T))
	r.loop.Tick(ctx)
	if n := len(r.f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("the tick at T claimed %d of 2 ready items, want 1 (the probe)", n)
	}
	r.loop.Tick(ctx) // the probe's read still unresolved
	if n := len(r.f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("claimed %d while the probe's read is unresolved, want 1", n)
	}
	close(gated.gate)
	claimed := r.f.CallsNamed("claimWorkItem")[0].Input["aggregateId"].(string)
	waitFor(t, func() bool { return r.f.Item(claimed)["status"] == "evaluating" })
	r.loop.wg.Wait()
	if len(r.h.jobs) != 1 {
		t.Fatalf("%d sessions after the reset, want the probe's", len(r.h.jobs))
	}
	if n := len(r.f.CallsNamed("releaseWorkItem")); n != 0 {
		t.Fatalf("%d releases after the reset", n)
	}
	// its read found headroom: the hold is cleared
	if _, _, held := r.store.Holds(projID, string(harness.KindBuild), AdapterClaudeCode); held {
		t.Fatal("the hold outlived the probe's successful read")
	}
}

// A held project's live direction is not offered for research while the
// hold stands — no claimDirectionResearch, the direction named on
// holding.unclaimed — and is offered once at its until.
func TestNoHeadroomHoldsResearchUntilItsUntil(t *testing.T) {
	r := newHeadroomRig(t, 2, nil)
	r.loop.Plan, r.loop.Dispatch = nil, func(context.Context, gateway.WorkItem) {}
	direction(r.f, dirID, projID, "nightly", "Tidy up.", 5)
	var researched atomic.Int32
	r.loop.Research = func(context.Context, gateway.Direction, gateway.Project, Scope) { researched.Add(1) }
	held := &ErrNoHeadroom{Org: "acme", Project: "mesh", ProjectID: projID, Kind: string(harness.KindResearch), Adapter: AdapterClaudeCode, Refused: []string{"max-issued"}, Until: r.T}
	r.store.NoteHeadroom(dirID, held)
	ctx := context.Background()

	r.loop.Tick(ctx)
	r.loop.wg.Wait()
	*r.now = r.now.Add(30 * time.Minute)
	r.loop.Tick(ctx)
	r.loop.wg.Wait()
	if n := len(r.f.CallsNamed("claimDirectionResearch")); n != 0 {
		t.Fatalf("research claimed %d time(s) under the hold", n)
	}
	var named bool
	for _, u := range r.loop.Holding().Unclaimed {
		named = named || (u.GoalID == dirID && u.Kind == "research" && strings.HasPrefix(u.Reason, held.Error()))
	}
	if !named {
		t.Fatalf("holding.unclaimed = %+v", r.loop.Holding().Unclaimed)
	}
	if n := strings.Count(r.logs.String(), held.Error()); n != 1 {
		t.Fatalf("said %d times:\n%s", n, r.logs)
	}

	*r.now = r.T
	r.limits.Observe("max-issued", fresh(r.T))
	r.loop.Tick(ctx)
	r.loop.wg.Wait()
	if n := len(r.f.CallsNamed("claimDirectionResearch")); n != 1 {
		t.Fatalf("research claimed %d time(s) at the hold's until, want 1", n)
	}
	// the hold kept items unheld throughout: research's own kind
	if n := len(r.f.CallsNamed("claimWorkItem")); n == 0 {
		t.Fatal("a research hold held the project's items")
	}
}

// A no-credential hold — kind "" — holds research too.
func TestNoCredentialHoldsResearch(t *testing.T) {
	r := newHeadroomRig(t, 2, nil)
	r.loop.Plan, r.loop.Dispatch = nil, func(context.Context, gateway.WorkItem) {}
	direction(r.f, dirID, projID, "nightly", "Tidy up.", 5)
	r.loop.Research = func(context.Context, gateway.Direction, gateway.Project, Scope) {}
	r.store.Note(dirID, &ErrNoCredential{Org: "acme", Project: "mesh", ProjectID: projID, Adapter: AdapterClaudeCode})
	r.loop.Tick(context.Background())
	r.loop.wg.Wait()
	if n := len(r.f.CallsNamed("claimDirectionResearch")); n != 0 {
		t.Fatalf("research claimed %d time(s) under a no-credential hold", n)
	}
}

// A hold of kind plan leaves the project's items claimable; a kind-""
// hold (no credential issued) holds both.
func TestNoHeadroomPlanHoldIsItsOwnKind(t *testing.T) {
	for _, c := range []struct {
		name                string
		note                func(*NoCredentials)
		wantItems, wantPlan int
	}{
		{"plan", func(s *NoCredentials) {
			s.NoteHeadroom(planningGoal2, &ErrNoHeadroom{Org: "acme", Project: "mesh", ProjectID: projID, Kind: string(harness.KindPlan), Adapter: AdapterClaudeCode, Until: time.Now().Add(time.Hour)})
		}, 1, 0},
		{"no credential", func(s *NoCredentials) {
			s.Note(planningGoal2, &ErrNoCredential{Org: "acme", Project: "mesh", ProjectID: projID, Adapter: AdapterClaudeCode})
		}, 0, 0},
	} {
		r := newHeadroomRig(t, 1, nil)
		var planned atomic.Int32
		r.loop.Plan = func(context.Context, gateway.Goal, Scope) { planned.Add(1) }
		r.loop.Dispatch = func(context.Context, gateway.WorkItem) {}
		c.note(r.store)
		r.loop.Tick(context.Background())
		r.loop.wg.Wait()
		if n := len(r.f.CallsNamed("claimWorkItem")); n != c.wantItems {
			t.Fatalf("%s hold: claimed %d item(s), want %d", c.name, n, c.wantItems)
		}
		if n := len(r.f.CallsNamed("claimPlan")); n != c.wantPlan || planned.Load() != 0 {
			t.Fatalf("%s hold: claimed %d plan(s), want %d", c.name, n, c.wantPlan)
		}
	}
}

// The planner refused for headroom counts no try, however often: a
// hold, not an unreadable plan.
func TestNoHeadroomPlannerCountsNoAttempt(t *testing.T) {
	r := newHeadroomRig(t, 1, nil)
	g, err := r.gw.Goal(context.Background(), planningGoal2)
	if err != nil {
		t.Fatal(err)
	}
	sc := scopesOf(r.loop.Cfg)[0]
	for i := 0; i < MaxPlanAttempts+1; i++ {
		err := r.pl.Plan(context.Background(), *g, sc)
		if _, ok := NoHeadroomOf(err); !ok {
			t.Fatalf("Plan = %v, want ErrNoHeadroom", err)
		}
	}
	if r.pl.attempts[planningGoal2] != 0 || strings.Contains(r.logs.String(), "no plan after") {
		t.Fatalf("attempts = %d:\n%s", r.pl.attempts[planningGoal2], r.logs)
	}
}

// An item this node holds in evaluating is not evaluated while its
// project's evaluations are held, and is at the hold's until.
func TestNoHeadroomHoldsEvaluationsUntilItsUntil(t *testing.T) {
	r := newHeadroomRig(t, 1, nil)
	r.loop.Plan, r.loop.Dispatch = nil, func(context.Context, gateway.WorkItem) {}
	r.f.WorkItems[item1]["status"], r.f.WorkItems[item1]["nodeId"] = "evaluating", r.loop.Cfg.NodeID
	var evaluated atomic.Int32
	r.loop.Evaluate = func(context.Context, gateway.WorkItem) { evaluated.Add(1) }
	r.store.NoteHeadroom(goalID, &ErrNoHeadroom{Org: "acme", Project: "mesh", ProjectID: projID, Kind: string(harness.KindEvaluate), Adapter: AdapterClaudeCode, Refused: []string{"max-issued"}, Until: r.T})
	r.loop.Tick(context.Background())
	r.loop.wg.Wait()
	if evaluated.Load() != 0 {
		t.Fatalf("evaluated %d time(s) under the hold", evaluated.Load())
	}
	*r.now = r.T
	r.loop.Tick(context.Background())
	r.loop.wg.Wait()
	if evaluated.Load() != 1 {
		t.Fatalf("evaluated %d time(s) at the hold's until, want 1", evaluated.Load())
	}
}
