package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// Credentials reads the org's own credentials on org_id and, for an org
// under an enterprise, the enterprise's on enterprise_id; another org's
// are never in it.
func TestCredentialsReadsTheOrgsAndItsEnterprises(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Orgs = []map[string]any{{"id": "org-1", "login": "acme", "enterpriseId": "ent-1"}}
	f.Credentials = []map[string]any{
		{"id": "c-org", "orgId": "org-1", "label": "max", "serves": []string{"claude-code"}, "projectIds": []string{"p-1"}, "status": "active"},
		{"id": "c-ent", "enterpriseId": "ent-1", "label": "team", "serves": []string{"claude-code", "codex"}, "status": "active"},
		{"id": "c-other", "orgId": "org-2", "label": "theirs", "serves": []string{"claude-code"}, "status": "active"},
	}
	gw := gateway.New(f.URL(), "rs_test")
	got, err := gw.Credentials(context.Background(), "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "c-org" || got[0].Enterprise() || got[1].ID != "c-ent" || !got[1].Enterprise() || !got[1].ServesAdapter("codex") {
		t.Fatalf("credentials %+v", got)
	}
}

// A platform without CredentialIndex.enterpriseId is read without it,
// and one without OrgIndex.enterpriseId has no enterprise to read.
func TestCredentialsOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["enterpriseId"] = true
	f.Orgs = []map[string]any{{"id": "org-1", "login": "acme"}}
	f.Credentials = []map[string]any{{"id": "c-org", "orgId": "org-1", "label": "max", "status": "active"}}
	gw := gateway.New(f.URL(), "rs_test")
	got, err := gw.Credentials(context.Background(), "org-1")
	if err != nil || len(got) != 1 || got[0].ID != "c-org" {
		t.Fatalf("credentials %+v, %v", got, err)
	}
}

// A platform with no credentialIndexs at all is Unsupported.
func TestCredentialIndexUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.FailReads["credentialIndexs"] = `Cannot query field "credentialIndexs" on type "Query".`
	_, err := gateway.New(f.URL(), "rs_test").Credentials(context.Background(), "org-1")
	if !gateway.IsUnsupported(err) {
		t.Fatalf("err %v, want unsupported", err)
	}
}
