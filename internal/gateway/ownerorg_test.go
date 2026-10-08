package gateway_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const ownerOrgID = "0a0a0a0a-2222-4222-8222-0a0a0a0a0a0a"

// TakesOwnerOrg is true only when RegisterNodeInput's owner field is an
// enum that has `org` (runsheet goal a-node-is-owned-by-an-org).
func TestTakesOwnerOrg(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*gatewaytest.Fake)
		want  bool
	}{
		{"enum with org", func(*gatewaytest.Fake) {}, true},
		{"enum with org, legacy spelling", func(f *gatewaytest.Fake) { f.OwnerFieldLegacy = true }, true},
		{"person and enterprise", func(f *gatewaytest.Fake) { f.OwnerKinds = []string{"person", "enterprise"} }, false},
		{"no owner field", func(f *gatewaytest.Fake) {
			f.UnknownFields["owner"] = true
			f.UnknownFields["ownerKind"] = true
		}, false},
		{"probe refused", func(f *gatewaytest.Fake) { f.Unsupported["__type"] = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			tc.setup(f)
			gw := gateway.New(f.URL(), "rs_test")
			if got := gw.TakesOwnerOrg(context.Background()); got != tc.want {
				t.Fatalf("TakesOwnerOrg = %v, want %v", got, tc.want)
			}
		})
	}
}

// The enum probe is asked once within ScalingProbeEvery and again after.
func TestTakesOwnerOrgAskedOncePerProbeWindow(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	now := time.Now()
	gw.Now = func() time.Time { return now }
	ctx := context.Background()
	enumReads := func() int {
		n := 0
		for _, r := range f.Reads {
			if r.Name == "__type NodeOwnerKind" {
				n++
			}
		}
		return n
	}
	for range 3 {
		if !gw.TakesOwnerOrg(ctx) {
			t.Fatal("TakesOwnerOrg = false, want true")
		}
	}
	if n := enumReads(); n != 1 {
		t.Fatalf("enum probed %d times within ScalingProbeEvery, want 1", n)
	}
	before := f.Requests()
	gw.TakesOwnerOrg(ctx)
	if n := f.Requests() - before; n != 0 {
		t.Fatalf("a cached TakesOwnerOrg sent %d requests, want 0", n)
	}
	now = now.Add(gateway.ScalingProbeEvery)
	gw.TakesOwnerOrg(ctx)
	if n := enumReads(); n != 2 {
		t.Fatalf("enum probed %d times after ScalingProbeEvery, want 2", n)
	}
}

// A registerNode with ownerKind org answers the org as owner, exactly
// that org as scope, and the node id the platform derives.
func TestRegisterNodeOwnerOrgAnswer(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Orgs = []map[string]any{
		{"id": ownerOrgID, "login": "grapevine", "slug": "grapevine", "name": "Grapevine"},
		{"id": "0b0b0b0b-3333-4333-8333-0b0b0b0b0b0b", "login": "runsheet", "slug": "runsheet", "name": "Runsheet"},
	}
	gw := gateway.New(f.URL(), "rs_test")
	want := gateway.NodeOwnerID("org", uuid.MustParse(ownerOrgID), "mini")
	a, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{
		NodeID: want.String(), OwnerID: ownerOrgID, Owner: "org", Name: "mini", Slug: "mini", Capacity: 1, Version: "test",
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if a == nil {
		t.Fatal("RegisterNode answered nil")
	}
	if a.NodeID != want.String() {
		t.Fatalf("nodeId = %s, want NodeOwnerID(org) %s", a.NodeID, want)
	}
	if a.Owner.Kind != "org" || a.Owner.ID != ownerOrgID || a.Owner.Name != "Grapevine" {
		t.Fatalf("owner = %+v, want org %s Grapevine", a.Owner, ownerOrgID)
	}
	if len(a.Scopes) != 1 || a.Scopes[0].OrgID != ownerOrgID || a.Scopes[0].Login != "grapevine" {
		t.Fatalf("scopes = %+v, want exactly grapevine", a.Scopes)
	}
	if got := f.Nodes[want.String()]["ownerKind"]; got != "org" {
		t.Fatalf("row ownerKind = %v, want org", got)
	}
	if f.SignInCalls != 0 {
		t.Fatalf("signIn called %d times, want 0", f.SignInCalls)
	}
}

// A gateway whose ownerKind enum lacks org refuses the registration, as
// GraphQL refuses an enum value it does not have — the node says which
// gateway it needs (OwnerOrgGateway).
func TestRegisterNodeOwnerOrgRefusedWithoutOwnerKindOrg(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.OwnerKinds = []string{"person", "enterprise"}
	f.Orgs = []map[string]any{{"id": ownerOrgID, "login": "grapevine"}}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	if gw.TakesOwnerOrg(ctx) {
		t.Fatalf("TakesOwnerOrg = true on a gateway before %s", gateway.OwnerOrgGateway)
	}
	_, err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{
		NodeID:  gateway.NodeOwnerID("org", uuid.MustParse(ownerOrgID), "mini").String(),
		OwnerID: ownerOrgID, Owner: "org", Name: "mini", Slug: "mini", Capacity: 1, Version: "test",
	})
	if err == nil || !strings.Contains(err.Error(), `"NodeOwnerKind" enum`) {
		t.Fatalf("RegisterNode err = %v, want the enum refusal", err)
	}
	if len(f.Nodes) != 0 {
		t.Fatalf("refused registration made rows: %v", f.Nodes)
	}
}

// The gateway the node needs for an org-owned node is named in one
// place, the platform goal until it ships.
func TestOwnerOrgGatewayNamed(t *testing.T) {
	if !strings.Contains(gateway.OwnerOrgGateway, "a-node-is-owned-by-an-org") {
		t.Fatalf("OwnerOrgGateway = %q, want it to name runsheet goal a-node-is-owned-by-an-org", gateway.OwnerOrgGateway)
	}
}

// The fake counts signIn so a registration can be shown never to call it.
func TestOwnerOrgFakeCountsSignIn(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	for i := 1; i <= 2; i++ {
		if _, err := gw.SignIn(context.Background()); err != nil {
			t.Fatalf("SignIn: %v", err)
		}
		if f.SignInCalls != i {
			t.Fatalf("SignInCalls = %d, want %d", f.SignInCalls, i)
		}
	}
}
