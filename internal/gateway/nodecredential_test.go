package gateway_test

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// machineID is a Latchkey machine id, as `mesh node login` writes it to
// ~/.mesh/token's machineId.
const machineID = "86e228b4-5c1d-4e2a-9b3f-0a1b2c3d4e5f"

// registerWithCredential registers a node on f with the given
// credential id and returns the one recorded registerNode input.
func registerWithCredential(t *testing.T, f *gatewaytest.Fake, credentialID string) map[string]any {
	t.Helper()
	gw := gateway.New(f.URL(), "rs_test")
	_, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{
		NodeID: "22222222-2222-4222-8222-222222222222", OrgID: orgID, OwnerID: f.UserID,
		Name: "mini", Slug: "mini", CredentialID: credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("registerNode")
	if len(calls) != 1 {
		t.Fatalf("registerNode calls = %+v", calls)
	}
	return calls[0].Input
}

// A machine's registration names its own credential when the schema
// takes credentialId, and the row records it. The bearer is the machine
// (NodeCredentialID): a person's bearer sending credentialId is refused
// by the node-mutations rule.
func TestRegisterNodeSendsCredentialIDWhenSetAndTaken(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.TakesNodeCredential = true
	f.NodeCredentialID = machineID
	in := registerWithCredential(t, f, machineID)
	if in[gateway.NodeCredentialField] != machineID {
		t.Fatalf("registerNode credentialId = %v, want %s: %+v", in[gateway.NodeCredentialField], machineID, in)
	}
	if row := f.Nodes["22222222-2222-4222-8222-222222222222"]; row[gateway.NodeCredentialField] != machineID {
		t.Fatalf("node row = %+v, want credentialId %s", row, machineID)
	}
}

// A person's registration names no credential.
func TestRegisterNodeLeavesCredentialIDOutWhenEmpty(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.TakesNodeCredential = true
	in := registerWithCredential(t, f, "")
	if _, ok := in[gateway.NodeCredentialField]; ok {
		t.Fatalf("registerNode sent credentialId with none set: %+v", in)
	}
}

// A gateway before runsheet#2290 has no credentialId on
// RegisterNodeInput: the probe leaves it out and the registration
// still goes through.
func TestRegisterNodeLeavesCredentialIDOutWhenTheSchemaLacksIt(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	in := registerWithCredential(t, f, machineID)
	if _, ok := in[gateway.NodeCredentialField]; ok {
		t.Fatalf("registerNode sent credentialId the schema does not take: %+v", in)
	}
	probes := 0
	for _, r := range f.Reads {
		if r.Name == "__type RegisterNodeInput" {
			probes++
		}
	}
	if probes != 1 {
		t.Fatalf("RegisterNodeInput probes = %d, want 1: %+v", probes, f.Reads)
	}
}

// A machine bearer naming another credential is refused, as the
// platform's registersNode machine branch refuses it, and the refusal
// is recorded.
func TestRegisterNodeOnAnotherCredentialIsNotYours(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.TakesNodeCredential = true
	f.NodeCredentialID = "aaaaaaaa-0000-4000-8000-000000000000"
	gw := gateway.New(f.URL(), "rs_test")
	_, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{
		NodeID: "22222222-2222-4222-8222-222222222222", OrgID: orgID, OwnerID: f.UserID,
		Name: "mini", Slug: "mini", CredentialID: machineID,
	})
	if err == nil || !strings.Contains(err.Error(), "not yours") {
		t.Fatalf("RegisterNode on another credential = %v, want not yours", err)
	}
	if calls := f.CallsNamed("registerNode"); len(calls) != 1 || calls[0].Input[gateway.NodeCredentialField] != machineID {
		t.Fatalf("refused registerNode not recorded as sent: %+v", calls)
	}
	if len(f.Nodes) != 0 {
		t.Fatalf("a refused registration left rows: %+v", f.Nodes)
	}
}
