package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// NotYoursLine names what was sent and where to compare it, only for a
// `not yours` refusal on a machine credential.
func TestNotYoursLine(t *testing.T) {
	refused := errors.New("register (owner person): registerNode: not yours")
	for _, tc := range []struct {
		name    string
		err     error
		id      string
		machine bool
		want    string
	}{
		{"nil error", nil, "86e228b4", true, ""},
		{"another refusal", errors.New("registerNode: forbidden"), "86e228b4", true, ""},
		{"console token", refused, "", false, ""},
		{"machine with an id", refused, "86e228b4", true,
			"sent credentialId 86e228b4 (the machine id in ~/.mesh/token); compare it with the node row's credential_id on the console (Nodes → mini); a row naming another credential needs mesh node login on that machine or the row's credential cleared"},
		{"machine with no id", refused, "", true,
			"sent no credentialId: the token file has no machineId and the access token's sub is not machine:<id>; run mesh node login on this machine, then compare its machine id with the node row's credential_id on the console (Nodes → mini)"},
	} {
		if got := NotYoursLine(tc.err, tc.id, tc.machine, "mini"); got != tc.want {
			t.Errorf("%s: NotYoursLine = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// fakeRegistrar registers on the owner contract through a real client
// on f, sending credID as credentialId.
func fakeRegistrar(f *gatewaytest.Fake, credID string, machine bool) Registrar {
	gw := gateway.New(f.URL(), f.Token)
	return Registrar{
		CredentialID:  credID,
		Machine:       machine,
		TakesOwner:    func(context.Context) bool { return true },
		ConfiguredOrg: func(context.Context) (string, error) { return "33333333-3333-4333-8333-333333333333", nil },
		RegisterOwner: func(ctx context.Context, nodeID, orgID, ownerID string) (*gateway.NodeAnswer, error) {
			return gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: nodeID, OrgID: orgID, OwnerID: ownerID, Name: "mini", Slug: "mini",
				Owner: "person", CredentialID: credID})
		},
	}
}

// A machine whose token names another credential than the gateway's
// bearer is refused `not yours`, and Register's error — what node.log
// gets — names the credentialId it sent and the row's credential_id.
func TestRegisterNotYoursNamesTheCredentialSent(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.TakesNodeCredential = true
	f.NodeCredentialID = "aaaaaaaa-0000-4000-8000-000000000000"
	const sent = "86e228b4-5c1d-4e2a-9b3f-0a1b2c3d4e5f"
	_, err := Register(context.Background(), fakeRegistrar(f, sent, true), ownedCfg(), nil)
	if err == nil {
		t.Fatal("Register on another credential succeeded")
	}
	for _, want := range []string{"not yours", "sent credentialId " + sent, "credential_id", "Nodes → mini"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Register error %q lacks %q", err, want)
		}
	}
}

// A machine token that yielded no id sends none, is refused, and says
// why none was sent.
func TestRegisterNotYoursWithNoCredentialID(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.TakesNodeCredential = true
	f.NodeCredentialID = "aaaaaaaa-0000-4000-8000-000000000000"
	_, err := Register(context.Background(), fakeRegistrar(f, "", true), ownedCfg(), nil)
	if err == nil || !strings.Contains(err.Error(), "not yours") || !strings.Contains(err.Error(), "sent no credentialId") {
		t.Fatalf("Register error = %v, want not yours and sent no credentialId", err)
	}
}
