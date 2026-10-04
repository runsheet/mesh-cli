package node

import (
	"context"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// rowRig is a node on seed's fake that knows one issued
// subscription, max-issued (cred-claude), with a reading in Limits, and
// whose credentialIndexs serve that credential's row active and allowed
// on any node until a test changes it.
type rowRig struct {
	f      *gatewaytest.Fake
	gw     *gateway.Client
	loop   *Loop
	known  *KnownAccounts
	limits *Limits
	logs   *syncBuf
	now    time.Time
}

func newRowRig(t *testing.T) *rowRig {
	t.Helper()
	f := seed(t)
	f.Credentials = []map[string]any{{"id": "cred-claude", "label": "max-issued", "orgId": orgID, "vendor": "anthropic", "kind": "subscription",
		"serves": []any{"claude-code"}, "projectIds": []any{projID}, "allowedNodeIds": []any{}, "status": "active"}}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1), OwnerID: f.UserID}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: 1, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	r := &rowRig{f: f, gw: gw, known: &KnownAccounts{}, limits: &Limits{}, logs: &syncBuf{}, now: time.Now().UTC().Truncate(time.Second)}
	if err := r.known.Note(AccountConfig{Label: "max-issued", Kind: AccountSubscription, ID: "cred-claude"}, r.now); err != nil {
		t.Fatal(err)
	}
	r.limits.Observe("max-issued", fresh(r.now))
	r.loop = &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: log.New(r.logs, "", 0),
		Known: r.known, Limits: r.limits, Now: func() time.Time { return r.now },
		Dispatch: func(context.Context, gateway.WorkItem) {}, Plan: func(context.Context, gateway.Goal, Scope) {}}
	return r
}

// tick runs one Tick to its end.
func (r *rowRig) tick(ctx context.Context) {
	r.loop.Tick(ctx)
	r.loop.wg.Wait()
}

// reported is the accounts the last heartbeat named in holding.accounts
// and in limits.
func (r *rowRig) reported(t *testing.T) (holding, limits []string) {
	t.Helper()
	beats := r.f.CallsNamed("heartbeatNode")
	if len(beats) == 0 {
		t.Fatal("no heartbeat")
	}
	in := beats[len(beats)-1].Input
	if h, ok := in["holding"].(map[string]any); ok {
		accts, _ := h["accounts"].([]any)
		for _, a := range accts {
			holding = append(holding, a.(map[string]any)["account"].(string))
		}
	}
	lim, _ := in["limits"].([]any)
	for _, a := range lim {
		limits = append(limits, a.(map[string]any)["account"].(string))
	}
	return holding, limits
}

// withdrawnByRow: a known label reported on one heartbeat, whose
// credentialIndexs row then turns against this node, is absent from
// the next heartbeat's holding and limits, and node.log says why.
func withdrawnByRow(t *testing.T, turn func(row map[string]any), why string) {
	r := newRowRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r.tick(ctx) // the pass builds a holding naming max-issued
	r.tick(ctx) // and this heartbeat carries it
	holding, limits := r.reported(t)
	if !slices.Contains(holding, "max-issued") || !slices.Contains(limits, "max-issued") {
		t.Fatalf("before: holding %v, limits %v; want max-issued in both", holding, limits)
	}

	turn(r.f.Credentials[0])
	r.tick(ctx)
	holding, limits = r.reported(t)
	if slices.Contains(holding, "max-issued") || slices.Contains(limits, "max-issued") {
		t.Fatalf("after: holding %v, limits %v; want max-issued in neither", holding, limits)
	}
	if want := "accounts: max-issued withdrawn (platform: " + why + ")"; strings.Count(r.logs.String(), want) != 1 {
		t.Fatalf("node.log wants %q once:\n%s", want, r.logs.String())
	}
	if accts := r.known.Accounts(r.now); len(accts) != 0 {
		t.Fatalf("known accounts = %+v, want none", accts)
	}
}

func TestTickWithdrawsADisabledCredential(t *testing.T) {
	withdrawnByRow(t, func(row map[string]any) { row["status"] = "disabled" }, "disabled")
}

func TestTickWithdrawsACredentialNotAllowedOnThisNode(t *testing.T) {
	withdrawnByRow(t, func(row map[string]any) { row["allowedNodeIds"] = []any{"another-node"} }, "not allowed on this node")
}

// A platform that refuses credentialIndexs: Tick withdraws nothing and
// says the fallback once, and the claim-time read that no longer names
// the label withdraws it (Credentials.Open's Reconcile, item 1).
func TestTickFallsBackToTheClaimReadWhenCredentialsAreRefused(t *testing.T) {
	r := newRowRig(t)
	r.f.FailReads["credentialIndexs"] = "not authorized: CredentialIndex"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r.tick(ctx)
	r.tick(ctx)
	holding, limits := r.reported(t)
	if !slices.Contains(holding, "max-issued") || !slices.Contains(limits, "max-issued") {
		t.Fatalf("holding %v, limits %v; want max-issued still in both", holding, limits)
	}
	if n := strings.Count(r.logs.String(), CredentialReadRefused); n != 1 {
		t.Fatalf("the fallback line %d times, want once:\n%s", n, r.logs.String())
	}
	if strings.Contains(r.logs.String(), "withdrawn") {
		t.Fatalf("withdrew on a refused read:\n%s", r.logs.String())
	}

	// the claim-time read now issues another credential, not max-issued
	r.f.DefaultSessionCredentials = []map[string]any{{"id": "cred-new", "label": "max-new", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": "sk-ant-oat01-NEW"}}
	creds := &Credentials{GW: r.gw, Known: r.known, Limits: r.limits, Now: func() time.Time { return r.now }, Log: log.New(r.logs, "", 0)}
	sctx := withSubject(ctx, scopesOf(r.loop.Cfg)[0], projID, "mesh")
	s, err := creds.Open(sctx, gateway.SessionRef{Kind: "build", ItemID: item1}, r.loop.Cfg.NodeID, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Cleanup()
	if !strings.Contains(r.logs.String(), "accounts: max-issued withdrawn (not issued by the mesh read)") {
		t.Fatalf("the claim-time read did not withdraw max-issued:\n%s", r.logs.String())
	}
	r.tick(ctx)
	holding, limits = r.reported(t)
	if slices.Contains(holding, "max-issued") || slices.Contains(limits, "max-issued") {
		t.Fatalf("after the claim read: holding %v, limits %v; want max-issued in neither", holding, limits)
	}
}
