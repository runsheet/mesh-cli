package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const enterpriseID = "eeeeeeee-1111-4111-8111-eeeeeeeeeeee"

// The platform spells RegisterNodeInput's owner kind `ownerKind`; a
// platform before registernode-answers-with-the-node-it-made spelled it
// `owner`. Either is an owner field; neither is a platform without one
// (ADR-015, The owner field is ownerKind).
func TestOwnerFieldSpellings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*gatewaytest.Fake)
		field  string
		owners bool
	}{
		{"ownerKind alone", func(*gatewaytest.Fake) {}, "ownerKind", true},
		{"owner alone", func(f *gatewaytest.Fake) { f.OwnerFieldLegacy = true }, "owner", true},
		{"neither", func(f *gatewaytest.Fake) {
			f.UnknownFields["owner"] = true
			f.UnknownFields["ownerKind"] = true
		}, "", false},
		{"legacy hidden", func(f *gatewaytest.Fake) {
			f.OwnerFieldLegacy = true
			f.UnknownFields["owner"] = true
		}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			tc.setup(f)
			gw := gateway.New(f.URL(), "rs_test")
			ctx := context.Background()
			if got := gw.OwnerField(ctx); got != tc.field {
				t.Fatalf("OwnerField = %q, want %q", got, tc.field)
			}
			if got := gw.TakesOwner(ctx); got != tc.owners {
				t.Fatalf("TakesOwner = %v, want %v", got, tc.owners)
			}
		})
	}
}

// RegisterNode sends the owner kind under the field the schema has,
// never both spellings, and always ownerId.
func TestRegisterNodeSendsTheOwnerFieldTheSchemaHas(t *testing.T) {
	for _, tc := range []struct {
		legacy    bool
		sent, not string
	}{
		{false, "ownerKind", "owner"},
		{true, "owner", "ownerKind"},
	} {
		t.Run(tc.sent, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			f.OwnerFieldLegacy = tc.legacy
			gw := gateway.New(f.URL(), "rs_test")
			_, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{
				NodeID: "22222222-2222-4222-8222-222222222222", OrgID: orgID, OwnerID: f.UserID,
				Name: "mini", Slug: "mini", Owner: "person",
			})
			if err != nil {
				t.Fatal(err)
			}
			calls := f.CallsNamed("registerNode")
			if len(calls) != 1 {
				t.Fatalf("registerNode calls = %+v", calls)
			}
			in := calls[0].Input
			if in[tc.sent] != "person" {
				t.Fatalf("registerNode %s = %v, want person: %+v", tc.sent, in[tc.sent], in)
			}
			if _, both := in[tc.not]; both {
				t.Fatalf("registerNode sent %s beside %s: %+v", tc.not, tc.sent, in)
			}
			if in["ownerId"] != f.UserID {
				t.Fatalf("registerNode ownerId = %v, want %s", in["ownerId"], f.UserID)
			}
		})
	}
}

// An enterprise registration is the enterprise admin's alone: the fake
// refuses an ownerId the caller does not admin, as the platform's
// adminOnEnterprise(ownerId) does, and keeps the row's owner when it
// does.
func TestRegisterNodeAsAnEnterpriseNeedsItsAdmin(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Enterprises[enterpriseID] = gatewaytest.Enterprise{Slug: "anythink", DisplayName: "Anythink", Admins: []string{"someone-else"}}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	in := gateway.RegisterNodeInput{
		NodeID: "22222222-2222-4222-8222-222222222222", OrgID: orgID, OwnerID: enterpriseID,
		Name: "mini", Slug: "mini", Owner: "enterprise",
	}
	_, err := gw.RegisterNode(ctx, in)
	if err == nil || !strings.Contains(err.Error(), "adminOnEnterprise") {
		t.Fatalf("RegisterNode by a non-admin = %v, want an adminOnEnterprise refusal", err)
	}
	if len(f.Nodes) != 0 {
		t.Fatalf("a refused registration left rows: %+v", f.Nodes)
	}

	f.Enterprises[enterpriseID] = gatewaytest.Enterprise{Slug: "anythink", DisplayName: "Anythink", Admins: []string{f.UserID}}
	if _, err := gw.RegisterNode(ctx, in); err != nil {
		t.Fatal(err)
	}
	row := f.Nodes[in.NodeID]
	if row["ownerKind"] != "enterprise" || row["ownerId"] != enterpriseID || row["orgId"] != orgID {
		t.Fatalf("node row = %+v", row)
	}
}

// MyEnterprises lists the enterprises the caller admins, from the fake.
func TestMyEnterprises(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Enterprises[enterpriseID] = gatewaytest.Enterprise{Slug: "anythink", DisplayName: "Anythink", Admins: []string{f.UserID}}
	f.Enterprises["ffffffff-1111-4111-8111-ffffffffffff"] = gatewaytest.Enterprise{Slug: "other", DisplayName: "Other", Admins: []string{"someone-else"}}
	gw := gateway.New(f.URL(), "rs_test")
	es, err := gw.MyEnterprises(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].ID != enterpriseID || es[0].Slug != "anythink" || es[0].DisplayName != "Anythink" {
		t.Fatalf("MyEnterprises = %+v", es)
	}
}

// myEnterprises is a Map scalar: a JSON array, or a string holding one.
func TestMyEnterprisesDecodesAStringHoldingTheArray(t *testing.T) {
	fixture := `[{"id":"` + enterpriseID + `","slug":"anythink","displayName":"Anythink","tenantSlug":"anythink","status":"active"}]`
	for name, payload := range map[string]any{"array": json.RawMessage(fixture), "string": fixture} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"myEnterprises": payload}})
			}))
			defer srv.Close()
			es, err := gateway.New(srv.URL, "rs_test").MyEnterprises(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := gateway.Enterprise{ID: enterpriseID, Slug: "anythink", DisplayName: "Anythink", TenantSlug: "anythink", Status: "active"}
			if len(es) != 1 || es[0] != want {
				t.Fatalf("MyEnterprises = %+v, want [%+v]", es, want)
			}
		})
	}
}

// RetireNode marks the row retired; an unknown id is refused.
func TestRetireNode(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	const id = "22222222-2222-4222-8222-222222222222"
	if _, err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: id, OrgID: orgID, Name: "mini", Slug: "mini"}); err != nil {
		t.Fatal(err)
	}
	if err := gw.RetireNode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := f.Nodes[id]["status"]; got != "retired" {
		t.Fatalf("node status = %v, want retired", got)
	}
	if err := gw.RetireNode(ctx, "33333333-3333-4333-8333-333333333333"); err == nil || !strings.Contains(err.Error(), "no such node") {
		t.Fatalf("RetireNode of an unknown id = %v, want no such node", err)
	}
}
