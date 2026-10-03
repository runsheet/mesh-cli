package node

import (
	"bytes"
	"context"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// replayRegistrar is mesh node start's registrar on a gateway client:
// the probe, the person's orgs restricted by node.yaml (ResolveScopes),
// the person's first org as AnyOrg — as v0.2.264 sent it — and the one
// owner registration.
func replayRegistrar(gw *gateway.Client, cfg *Config) Registrar {
	return Registrar{
		TakesOwner: gw.TakesOwner,
		Resolve:    func(ctx context.Context) (Scopes, error) { return ResolveScopes(ctx, gw, cfg) },
		AnyOrg: func(ctx context.Context) (string, error) {
			orgs, err := gw.MyOrgs(ctx)
			if err != nil || len(orgs) == 0 {
				return "", err
			}
			return orgs[0].ID, nil
		},
		RegisterOwner: func(ctx context.Context, nodeID, orgID, ownerID string) (*gateway.NodeAnswer, error) {
			return gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: nodeID, OrgID: orgID, OwnerID: ownerID, Name: cfg.Name, Slug: cfg.Slug,
				Harnesses: []string{"claude-code"}, Capacity: cfg.Capacity.Max, Version: "v0.2.264", Owner: cfg.OwnerKind()})
		},
	}
}

// 2026-10-03 13:51Z, replayed: the grapevine node, per org until now
// with node.yaml's orgs [grapevine], comes up against a platform that
// takes ownerKind and answers registerNode and heartbeatNode with no
// scopes (the Fake's nil Answer), its person's first org foghornid. It
// registers on the owner contract and still serves grapevine — on
// grapevine's per-org id, the row that served it before — never none,
// and its first tick, a heartbeat under the owned id answered nil,
// claims grapevine's ready item.
func TestOwnedReplay20261003ClaimsGrapevine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const foghornid = "cccccccc-3333-4333-8333-cccccccccccc"
	f := seed(t)
	f.Orgs = []map[string]any{
		{"id": foghornid, "login": "foghornid", "accountId": "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"},
		{"id": orgID, "login": "", "tenantSlug": "grapevine", "accountId": acctID},
	}
	if f.Answer != nil || len(f.AnswerJSON) != 0 {
		t.Fatal("the replay's platform answers no scopes")
	}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{Slug: "grapevine", Name: "grapevine", OwnerID: f.UserID, Contract: ContractPerOrg, Orgs: []string{"grapevine"}, Capacity: Fixed(4)}
	if !gw.TakesOwner(context.Background()) {
		t.Fatal("the fake does not take ownerKind")
	}

	reg, err := Register(context.Background(), replayRegistrar(gw, cfg), cfg, cfg.Orgs)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Contract() != ContractOwner || reg.Owned == nil || reg.Answer != nil {
		t.Fatalf("registered %+v, want the owner contract on a nil answer", reg)
	}
	lines := strings.Join(RegistrationLines(cfg, reg), "\n")
	if !strings.Contains(lines, "serving grapevine") || strings.Contains(lines, "serving none") {
		t.Fatalf("registration lines:\n%s\nwant serving grapevine, never serving none", lines)
	}
	perOrg, err := cfg.NodeIDIn(orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Scopes) != 1 || reg.Scopes[0].NodeID != perOrg {
		t.Fatalf("scopes %+v, want grapevine on its per-org id %s", reg.Scopes, perOrg)
	}

	var buf bytes.Buffer
	var ran []string
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: reg.Scopes, Owned: reg.Owned, Version: "v0.2.264", Interval: time.Hour, Harness: "claude-code",
		Log: log.New(&buf, "", 0), Dispatch: func(_ context.Context, item gateway.WorkItem) { ran = append(ran, item.ID) }}
	loop.Reload = OwnedReloader(func() (*Config, error) { return cfg, nil })
	loop.TakeAnswer(reg.Answer)
	loop.Tick(context.Background())
	loop.wg.Wait()

	if beats := f.CallsNamed("heartbeatNode"); len(beats) != 1 {
		t.Fatalf("heartbeats %+v, want one under the owned id", beats)
	}
	if names := loop.CurrentScopes().Names(); !slices.Equal(names, []string{"grapevine"}) {
		t.Fatalf("after a nil heartbeat answer the loop serves %q, want grapevine\n%s", names, buf.String())
	}
	if n := len(f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("claimWorkItem %d times, want grapevine's item claimed once\n%s", n, buf.String())
	}
	if got := f.WorkItems[item1]["nodeId"]; got != perOrg {
		t.Fatalf("grapevine's item held by %v, want %s", got, perOrg)
	}
	if !slices.Equal(ran, []string{item1}) {
		t.Fatalf("dispatched %q, want grapevine's item", ran)
	}
}
