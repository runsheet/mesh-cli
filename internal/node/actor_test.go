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

func (g *actorGW) FinishWorkItem(ctx context.Context, itemID, branch, headSha string, costCents int, usage *gateway.UsageReport) error {
	g.note("finishWorkItem", ctx)
	return g.Gateway.FinishWorkItem(ctx, itemID, branch, headSha, costCents, usage)
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
