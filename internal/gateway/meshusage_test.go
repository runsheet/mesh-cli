package gateway_test

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A job's usage goes to Mesh on its job — by account reference, never a
// secret — and the node's accounts' windows on the node; a platform
// without the routes answers IsUnsupported, and an unknown job is a
// refusal, not a missing route (runsheet ADR 078 §5).
func TestMeshTakesAJobsUsageAndTheNodesAccounts(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimedItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	s, err := gw.OpenMeshSession(ctx, gateway.MeshSessionRequest{ItemID: itemID, ClaimSeq: 2, NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	rep := &gateway.UsageReport{CredentialID: "acct-1", Adapter: "claude-code", Model: "claude-opus-5-5", InputTokens: 10, OutputTokens: 4, CacheReadTokens: 7, ListCostCents: 120, WallMs: 9000, Turns: 3}
	if err := gw.ReportJobUsage(ctx, nodeID, s.SessionID, gateway.JobUsageOf(rep)); err != nil {
		t.Fatal(err)
	}
	got := f.JobUsages[s.SessionID]
	if len(got) != 1 || got[0]["account_ref"] != "acct-1" || got[0]["tokens_input"] != float64(10) || got[0]["cost_list_cents"] != float64(120) || got[0]["turns"] != float64(3) || got[0]["duration_ms"] != float64(9000) {
		t.Fatalf("usage: %+v", got)
	}

	err = gw.ReportJobUsage(ctx, nodeID, "no-such-job", gateway.JobUsageOf(rep))
	if err == nil || gateway.IsUnsupported(err) {
		t.Fatalf("an unknown job is a refusal: %v", err)
	}

	at := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	accounts := []gateway.NodeAccount{{AccountRef: "acct-1", Overage: "rejected", Windows: []gateway.AccountWindow{{Window: "five_hour", UsedPct: 41, ResetsAt: at.Add(3 * time.Hour)}}}}
	if err := gw.ReportNodeAccounts(ctx, nodeID, accounts, at); err != nil {
		t.Fatal(err)
	}
	if a := f.NodeAccounts[nodeID]; len(a) != 1 || a[0]["seen_at"] != "2026-10-28T01:00:00Z" {
		t.Fatalf("accounts: %+v", a)
	}

	f.NoMeshUsage = true
	if err := gw.ReportJobUsage(ctx, nodeID, s.SessionID, gateway.JobUsageOf(rep)); !gateway.IsUnsupported(err) {
		t.Fatalf("a platform without the route: %v", err)
	}
	if err := gw.ReportNodeAccounts(ctx, nodeID, accounts, at); !gateway.IsUnsupported(err) {
		t.Fatalf("a platform without the route: %v", err)
	}
}
