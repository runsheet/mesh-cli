package node

import (
	"context"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// actorGW records, per command, the actor gateway.ActorFrom answers on
// the ctx the node sent it on ("" for none).
type actorGW struct {
	Gateway
	mu     sync.Mutex
	actors map[string][]string
}

func (g *actorGW) note(field string, ctx context.Context) {
	a, _ := gateway.ActorFrom(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.actors == nil {
		g.actors = map[string][]string{}
	}
	g.actors[field] = append(g.actors[field], a)
}

func (g *actorGW) of(field string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.actors[field]...)
}

func (g *actorGW) ClaimWorkItem(ctx context.Context, itemID, nodeID, h string) error {
	g.note("claimWorkItem", ctx)
	return g.Gateway.ClaimWorkItem(ctx, itemID, nodeID, h)
}

func (g *actorGW) ReportWorkItemProgress(ctx context.Context, itemID, note string, costCents int, sessionID string) error {
	g.note("reportWorkItemProgress", ctx)
	return g.Gateway.ReportWorkItemProgress(ctx, itemID, note, costCents, sessionID)
}

func (g *actorGW) FinishWorkItem(ctx context.Context, itemID, branch, headSha string, costCents int, usage *gateway.UsageReport, description ...string) error {
	g.note("finishWorkItem", ctx)
	return g.Gateway.FinishWorkItem(ctx, itemID, branch, headSha, costCents, usage, description...)
}

// A build's claim, progress and finish act as agent:builder@<slug>, the
// node.yaml slug, else "node" (ADR-015's amendment 2026-10-03).
func TestBuildCommandsActAsTheBuilder(t *testing.T) {
	for _, tc := range []struct{ slug, want string }{
		{"test-node", "agent:builder@test-node"},
		{"", "agent:builder@node"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			f := seed(t)
			gw := gateway.New(f.URL(), "rs_test")
			cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: tc.slug, NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
			if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: 1, Version: "test"}); err != nil {
				t.Fatal(err)
			}
			rec := &actorGW{Gateway: gw}
			ws := &fakeWS{root: t.TempDir()}
			runner := &Runner{Credentials: testCreds(gw), GW: rec, Harness: &fakeHarness{}, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, MaxBudgetUSD: 5, ProgressEvery: 10 * time.Millisecond, Log: log.New(os.Stderr, "", 0)}
			loop := &Loop{GW: rec, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: runner.Log,
				Dispatch: func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			loop.Tick(ctx)
			waitFor(t, func() bool { return f.Item(item1)["status"] == "evaluating" })

			for _, field := range []string{"claimWorkItem", "reportWorkItemProgress", "finishWorkItem"} {
				got := rec.of(field)
				if len(got) == 0 {
					t.Fatalf("no %s call", field)
				}
				for _, a := range got {
					if a != tc.want {
						t.Fatalf("%s actors %q, want each %q", field, got, tc.want)
					}
				}
			}
		})
	}
}

func (g *actorGW) EvaluateWorkItem(ctx context.Context, itemID string, passed bool, notes string, clauses []gateway.FailedClause, costCents int, usage *gateway.UsageReport, choice gateway.EvaluateChoice) error {
	g.note("evaluateWorkItem", ctx)
	return g.Gateway.EvaluateWorkItem(ctx, itemID, passed, notes, clauses, costCents, usage, choice)
}

func (g *actorGW) EvaluateWorkItemNoVerdict(ctx context.Context, itemID, reason string, costCents int, usage *gateway.UsageReport, choice gateway.EvaluateChoice) error {
	g.note("evaluateWorkItem", ctx)
	return g.Gateway.EvaluateWorkItemNoVerdict(ctx, itemID, reason, costCents, usage, choice)
}

// JobBriefOf and JobKindOf pass the briefs and catalogue through, so a
// session started on the recorder reads its kind's brief as on the client.
func (g *actorGW) JobBriefOf(kind string, subject gateway.JobSubject) (*gateway.JobBrief, bool) {
	if r, ok := g.Gateway.(briefReader); ok {
		return r.JobBriefOf(kind, subject)
	}
	return nil, false
}

func (g *actorGW) JobKindOf(ctx context.Context, kind string) (gateway.JobKind, bool) {
	if r, ok := g.Gateway.(kindReader); ok {
		return r.JobKindOf(ctx, kind)
	}
	return gateway.JobKind{}, false
}

func (g *actorGW) FinishJob(ctx context.Context, nodeID, kind string, subject gateway.JobSubject, finish map[string]any) (map[string]any, error) {
	g.note("finishJob", ctx)
	return g.Gateway.FinishJob(ctx, nodeID, kind, subject, finish)
}

// An evaluation's job close (finishJob) acts as agent:evaluator@<slug>,
// whatever the session handed the product (ADR-015's amendment 2026-10-03).
func TestEvaluateActsAsTheEvaluator(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"verdict", "It holds.\n\nVERDICT: PASS"},
		{"no verdict", "I could not tell."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seed(t)
			gw := gateway.New(f.URL(), "rs_test")
			cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
			f.WorkItems[item1]["status"] = "evaluating"
			f.WorkItems[item1]["nodeId"] = cfg.NodeID
			f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
			rec := &actorGW{Gateway: gw}
			ws := &fakeWS{root: t.TempDir()}
			ev := &Evaluator{Credentials: testCreds(gw), GW: rec, Harness: &fakeHarness{text: tc.text}, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			item, err := gw.WorkItem(ctx, item1)
			if err != nil {
				t.Fatal(err)
			}
			if err := ev.Evaluate(ctx, *item); err != nil {
				t.Fatal(err)
			}
			got := rec.of("finishJob")
			if len(got) != 1 || got[0] != "agent:evaluator@test-node" {
				t.Fatalf("finishJob actors %q, want [agent:evaluator@test-node]", got)
			}
		})
	}
}

// A plan job's close (finishJob) acts as agent:planner@<slug> (ADR-015's
// amendment 2026-10-03).
func TestPlanActsAsThePlanner(t *testing.T) {
	f := seed(t)
	const goal2 = "dddddddd-3333-4333-8333-dddddddddddd"
	f.Goals[goal2] = map[string]any{"id": goal2, "orgId": orgID, "projectId": projID, "slug": "new-thing", "title": "New thing",
		"statement": "Build the new thing.", "acceptance": "the thing works", "budgetCents": 2000, "spentCents": 0, "status": "planning"}
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	rec := &actorGW{Gateway: gw}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"items\": [{\"seq\": 1, \"title\": \"Write the failing test\", \"brief\": \"A test in widgets.\", \"acceptance\": \"go test fails on TestThing\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"}
	pl := &Planner{Credentials: testCreds(gw), GW: rec, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
	loop := &Loop{GW: rec, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: pl.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Plan:     func(ctx context.Context, g gateway.Goal, sc Scope) { _ = pl.Plan(ctx, g, sc) }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()
	got := rec.of("finishJob")
	if len(got) != 1 || got[0] != "agent:planner@test-node" {
		t.Fatalf("finishJob actors %q, want [agent:planner@test-node]", got)
	}
}
