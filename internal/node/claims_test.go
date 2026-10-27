package node

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

const claimsNode = "22222222-2222-4222-8222-222222222222"

// claimsFake is the seed with the project's repo and a node config.
func claimsFake(t *testing.T) (*gatewaytest.Fake, *Config) {
	f := seed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Projects[projID]["maxLiveGoals"] = 10 // room for research beside the plans
	// no items: only plans and research are claimed here
	delete(f.WorkItems, item1)
	delete(f.WorkItems, item2)
	return f, &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: claimsNode, Capacity: Fixed(1), OwnerID: f.UserID}
}

func addPlanningGoal(f *gatewaytest.Fake, id, slug string) {
	f.Goals[id] = map[string]any{"id": id, "orgId": orgID, "projectId": projID, "slug": slug, "title": slug,
		"statement": "Build " + slug + ".", "acceptance": "it works", "budgetCents": 2000, "spentCents": 0, "status": "planning"}
}

// The record of a plan claim is on disk from the moment the claim is
// granted — before the session has opened, no state.json under the root
// — and gone once the plan is handed in.
func TestPlanClaimIsRecordedUntilHandIn(t *testing.T) {
	f, cfg := claimsFake(t)
	const goal = "dddddddd-3333-4333-8333-dddddddddddd"
	addPlanningGoal(f, goal, "new-thing")
	gw := gateway.New(f.URL(), "rs_test")
	ws := &fakeWS{root: t.TempDir()}
	h := &blockingHarness{release: make(chan struct{}), fakeHarness: fakeHarness{text: "```json\n{\"items\": [" +
		"{\"seq\": 1, \"title\": \"Do it\", \"brief\": \"Do it.\", \"acceptance\": \"go test ./... passes\", \"depends_on\": [], \"repo\": \"acme/widgets\"}" +
		"]}\n```"}}
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&strings.Builder{}, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: pl.Log, ClaimsDir: ClaimsDir(ws.root),
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Plan:     func(ctx context.Context, g gateway.Goal, sc Scope) { _ = pl.Plan(ctx, g, sc) }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	for h.started() == 0 && ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	if len(f.CallsNamed("claimPlan")) != 1 {
		t.Fatalf("claims: %+v", f.CallsNamed("claimPlan"))
	}
	claims, err := FindClaims(ws.root, []string{claimsNode})
	if err != nil || len(claims) != 1 || claims[0].Kind != harness.KindPlan || claims[0].GoalID != goal || claims[0].NodeID != claimsNode || claims[0].ScopeID != orgID || claims[0].ClaimedAt.IsZero() {
		t.Fatalf("claims while the session is unopened: %+v, %v", claims, err)
	}
	if found, _ := FindSessions(ws.root, []string{claimsNode}); len(found) != 0 {
		t.Fatalf("a session is open: %+v", found)
	}
	close(h.release)
	loop.wg.Wait()
	if len(f.CallsNamed("finishJob")) != 1 {
		t.Fatalf("plan not handed in")
	}
	if claims, _ := FindClaims(ws.root, []string{claimsNode}); len(claims) != 0 {
		t.Fatalf("record left after the hand-in: %+v", claims)
	}
}

// claimsLoop is a node on root whose plans and research block until
// their context is done (the credential read, the context fetch: work
// before the unit opens), each start sent on started.
func claimsLoop(f *gatewaytest.Fake, cfg *Config, root string, logs *strings.Builder, plan func(context.Context, gateway.Goal, Scope), research func(context.Context, gateway.Direction, gateway.Project, Scope)) *Loop {
	return &Loop{GW: gateway.New(f.URL(), "rs_test"), Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Harness: "claude-code",
		Log: log.New(logs, "", 0), ClaimsDir: ClaimsDir(root),
		Dispatch: func(context.Context, gateway.WorkItem) {}, Plan: plan, Research: research}
}

// After a restart — Restart, or a self-update's ErrRestartForUpdate —
// the claims the first process held but never opened are on disk, and a
// second process on the same root claims each again by the same node id
// and starts one fresh session for each, logging one line per claim; a
// claim whose session was opened is left to AdoptSessions.
func TestClaimsSurviveARestartAndAreReopened(t *testing.T) {
	for _, how := range []string{"restart", "update"} {
		t.Run(how, func(t *testing.T) {
			f, cfg := claimsFake(t)
			const g1, g2, g3 = "dddddddd-1111-4111-8111-000000000001", "dddddddd-1111-4111-8111-000000000002", "dddddddd-1111-4111-8111-000000000003"
			addPlanningGoal(f, g1, "one")
			addPlanningGoal(f, g2, "two")
			addPlanningGoal(f, g3, "opened")
			direction(f, dirID, projID, "nightly", "Tidy up.", 5)
			root := t.TempDir()

			var first strings.Builder
			var wg sync.WaitGroup
			wg.Add(4)
			wait := func(ctx context.Context) { wg.Done(); <-ctx.Done() }
			loop1 := claimsLoop(f, cfg, root, &first,
				func(ctx context.Context, _ gateway.Goal, _ Scope) { wait(ctx) },
				func(ctx context.Context, _ gateway.Direction, _ gateway.Project, _ Scope) { wait(ctx) })
			done := make(chan error, 1)
			go func() { done <- loop1.Run(context.Background()) }()
			opened := make(chan struct{})
			go func() { wg.Wait(); close(opened) }()
			select {
			case <-opened:
			case <-time.After(10 * time.Second):
				t.Fatalf("the first process did not start four sessions:\n%s", first.String())
			}
			if how == "restart" {
				loop1.Restart()
			} else {
				loop1.mu.Lock()
				loop1.restart = true
				loop1.mu.Unlock()
				loop1.detach()
				loop1.notify()
			}
			select {
			case err := <-done:
				want := ErrRestart
				if how == "update" {
					want = ErrRestartForUpdate
				}
				if !errors.Is(err, want) {
					t.Fatalf("Run = %v, want %v", err, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return")
			}
			claims, err := FindClaims(root, []string{claimsNode})
			if err != nil || len(claims) != 4 {
				t.Fatalf("claims after the %s: %+v, %v", how, claims, err)
			}
			// g3's session had opened: its state file is there
			sdir := filepath.Join(root, "plans", "opened.session")
			if err := os.MkdirAll(sdir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := sessionunit.WriteState(sdir, sessionunit.State{NodeID: claimsNode, GoalID: g3, Kind: string(harness.KindPlan), StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			found, _ := FindSessions(root, []string{claimsNode})
			if len(found) != 1 {
				t.Fatalf("found: %+v", found)
			}

			before := len(f.Calls)
			var second strings.Builder
			var mu sync.Mutex
			var planned, researched []string
			loop2 := claimsLoop(f, cfg, root, &second,
				func(_ context.Context, g gateway.Goal, _ Scope) {
					mu.Lock()
					planned = append(planned, g.Slug)
					mu.Unlock()
				},
				func(_ context.Context, d gateway.Direction, _ gateway.Project, _ Scope) {
					mu.Lock()
					researched = append(researched, d.Slug)
					mu.Unlock()
				})
			if n := loop2.ReopenClaims(context.Background(), claims, found); n != 3 {
				t.Fatalf("re-opened %d, want 3\n%s", n, second.String())
			}
			loop2.wg.Wait()
			slices.Sort(planned)
			if strings.Join(planned, ",") != "one,two" || strings.Join(researched, ",") != "nightly" {
				t.Fatalf("sessions: plans %v, research %v", planned, researched)
			}
			var reclaimed []string
			for _, c := range f.Calls[before:] {
				if c.Name != "claimPlan" && c.Name != "claimDirectionResearch" {
					continue
				}
				if c.Input["nodeId"] != claimsNode {
					t.Fatalf("re-claimed by another node: %+v", c)
				}
				reclaimed = append(reclaimed, c.Name+" "+c.Input["aggregateId"].(string))
			}
			slices.Sort(reclaimed)
			if want := []string{"claimDirectionResearch " + dirID, "claimPlan " + g1, "claimPlan " + g2}; !slices.Equal(reclaimed, want) {
				t.Fatalf("re-claims %v, want %v", reclaimed, want)
			}
			out := second.String()
			for _, want := range []string{"re-opening the plan claim of goal one held since ", "re-opening the plan claim of goal two held since ", "re-opening the research claim of mesh/nightly held since "} {
				if strings.Count(out, want) != 1 {
					t.Fatalf("want one %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "opened") {
				t.Fatalf("the opened claim was re-opened:\n%s", out)
			}
			// the re-opened sessions handed in: their records are gone; the
			// opened one's waits for its adopted session's end
			left, _ := FindClaims(root, []string{claimsNode})
			if len(left) != 1 || left[0].GoalID != g3 {
				t.Fatalf("records left: %+v", left)
			}
			// AdoptSessions takes the opened one; its handled end takes the record
			loop2.AdoptSession = func(context.Context, FoundSession) {}
			if n := loop2.AdoptSessions(context.Background(), found); n != 1 {
				t.Fatalf("adopted %d", n)
			}
			loop2.wg.Wait()
			if left, _ := FindClaims(root, []string{claimsNode}); len(left) != 0 {
				t.Fatalf("record left after the adopted session's end: %+v", left)
			}
		})
	}
}

// A re-claim the platform refuses — another node took the plan or the
// research after the TTL — removes the record and says why; so does a
// goal no longer in planning.
func TestRefusedReopenRemovesTheRecord(t *testing.T) {
	f, cfg := claimsFake(t)
	const g1, g2 = "dddddddd-1111-4111-8111-000000000001", "dddddddd-1111-4111-8111-000000000002"
	addPlanningGoal(f, g1, "taken")
	addPlanningGoal(f, g2, "planned")
	f.Goals[g1]["plannerNodeId"] = "33333333-3333-4333-8333-333333333333"
	f.Goals[g2]["status"] = "awaiting_approval"
	direction(f, dirID, projID, "nightly", "Tidy up.", 5)["researchNodeId"] = "33333333-3333-4333-8333-333333333333"
	root := t.TempDir()
	at := time.Date(2026, 10, 3, 3, 30, 35, 0, time.UTC)
	for _, r := range []ClaimRecord{
		{NodeID: claimsNode, Kind: harness.KindPlan, GoalID: g1, ScopeID: orgID, Name: "taken", ClaimedAt: at},
		{NodeID: claimsNode, Kind: harness.KindPlan, GoalID: g2, ScopeID: orgID, Name: "planned", ClaimedAt: at},
		{NodeID: claimsNode, Kind: harness.KindResearch, DirectionID: dirID, ScopeID: orgID, Name: "mesh/nightly", ClaimedAt: at},
	} {
		if err := WriteClaim(ClaimsDir(root), r); err != nil {
			t.Fatal(err)
		}
	}
	claims, _ := FindClaims(root, []string{claimsNode})
	var logs strings.Builder
	started := 0
	loop := claimsLoop(f, cfg, root, &logs,
		func(context.Context, gateway.Goal, Scope) { started++ },
		func(context.Context, gateway.Direction, gateway.Project, Scope) { started++ })
	if n := loop.ReopenClaims(context.Background(), claims, nil); n != 0 || started != 0 {
		t.Fatalf("re-opened %d, started %d\n%s", n, started, logs.String())
	}
	out := logs.String()
	for _, want := range []string{
		"re-opening the plan claim of goal taken refused: ",
		"re-opening the research claim of mesh/nightly refused: ",
		"the plan claim of goal planned: the goal is awaiting_approval, not planning; claim record removed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "claim record removed") != 3 {
		t.Fatalf("each record removed once:\n%s", out)
	}
	if left, _ := FindClaims(root, []string{claimsNode}); len(left) != 0 {
		t.Fatalf("records left: %+v", left)
	}
	if loop.sessions() != 0 {
		t.Fatal("a refused claim is still marked")
	}
}
