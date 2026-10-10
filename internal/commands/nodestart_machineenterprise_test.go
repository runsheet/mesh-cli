package commands

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// machineOperatorJWT is a machine access token whose operator.sub is
// the person the machine acts for, as Latchkey issues it.
func machineOperatorJWT(sub, operator string) string {
	payload, _ := json.Marshal(map[string]any{"sub": sub, "role": "node", "operator": map[string]any{"sub": operator}})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// machineEnterpriseFake is the platform for a fresh enterprise node on
// a machine credential (goal
// a-machine-registers-an-enterprise-node-without-a-person): the machine
// bearer's signIn is refused as the platform refuses it, the caller is
// the machine's operator, and anythink is administered by admins.
func machineEnterpriseFake(t *testing.T, access, operator string, admins ...string) *gatewaytest.Fake {
	t.Helper()
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	f.Token = access
	f.TakesNodeCredential = true
	f.NodeCredentialID = credMachine
	f.RefusesMachineSignIn = true
	f.UserID = operator
	f.Enterprises[miniAnythinkID] = gatewaytest.Enterprise{Slug: "anythink", DisplayName: "Anythink", Admins: admins}
	f.Orgs = []map[string]any{{"id": miniRunsheetID, "login": "runsheet"}}
	f.Answer = nil
	return f
}

// machineEnterpriseHome is a fresh HOME whose node.yaml is owner:
// enterprise, enterprise: anythink — no owner_id, node_id or
// enterprise_id — and whose only credential is the machine's.
func machineEnterpriseHome(t *testing.T, f *gatewaytest.Fake, token string) string {
	t.Helper()
	home := ownerOrgHome(t, f, token)
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte("slug: bob\nname: bob\nowner: enterprise\nenterprise: anythink\nsandbox: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func assertNoSignIn(t *testing.T, f *gatewaytest.Fake) {
	t.Helper()
	if f.SignInCalls != 0 {
		t.Errorf("signIn called %d times, want 0", f.SignInCalls)
	}
	for _, c := range f.Calls {
		if c.Name == "signIn" {
			t.Errorf("the fake recorded a signIn call: %+v", c)
		}
	}
}

// A fresh enterprise node with only a machine credential registers on
// its first start: the owner is the machine's operator, the enterprise
// resolved by myEnterprises, node.yaml gains node_id and enterprise_id,
// and signIn — refused for a machine bearer — is never called.
func TestStartMachineEnterpriseRegisters(t *testing.T) {
	access := machineOperatorJWT("machine:"+credMachine, miniAdminID)
	f := machineEnterpriseFake(t, access, miniAdminID, miniAdminID)
	home := machineEnterpriseHome(t, f, machineToken(t, access, credMachine))
	logs, err := startOnce()
	if err != nil {
		t.Fatalf("mesh node start: %v\n%s", err, logs)
	}
	y := readNodeYAML(t, home)
	if y["node_id"] == "" || y["enterprise_id"] != miniAnythinkID {
		t.Errorf("node.yaml node_id %q enterprise_id %q, want non-empty and %s", y["node_id"], y["enterprise_id"], miniAnythinkID)
	}
	ins := registerInputs(t, f)
	if len(ins) == 0 {
		t.Fatal("no registerNode sent")
	}
	for _, in := range ins {
		if in["ownerKind"] != "enterprise" || in["ownerId"] != miniAnythinkID {
			t.Errorf("registerNode ownerKind %v ownerId %v, want enterprise %s", in["ownerKind"], in["ownerId"], miniAnythinkID)
		}
	}
	assertNoSignIn(t, f)
	if strings.Contains(logs, "a person's own") {
		t.Errorf("node.log names signIn's refusal:\n%s", logs)
	}
}

// The token file's userId names the operator as well as the claim does:
// a machine token whose access token carries no operator still
// registers, on the userId `mesh node login` saved.
func TestStartMachineEnterpriseRegistersOnTheSavedOperator(t *testing.T) {
	access := machineJWT("machine:" + credMachine)
	f := machineEnterpriseFake(t, access, miniAdminID, miniAdminID)
	raw, _ := json.Marshal(map[string]any{"kind": "machine", "accessToken": access, "machineId": credMachine, "userId": miniAdminID})
	home := machineEnterpriseHome(t, f, string(raw))
	logs, err := startOnce()
	if err != nil {
		t.Fatalf("mesh node start: %v\n%s", err, logs)
	}
	if y := readNodeYAML(t, home); y["node_id"] == "" || y["enterprise_id"] != miniAnythinkID {
		t.Errorf("node.yaml node_id %q enterprise_id %q", y["node_id"], y["enterprise_id"])
	}
	assertNoSignIn(t, f)
}

// A machine whose operator does not admin the enterprise is refused by
// the enterprise's name, with no signIn and no registerNode.
func TestStartMachineEnterpriseNotAdmin(t *testing.T) {
	access := machineOperatorJWT("machine:"+credMachine, miniNonAdminID)
	f := machineEnterpriseFake(t, access, miniNonAdminID, miniAdminID)
	home := machineEnterpriseHome(t, f, machineToken(t, access, credMachine))
	logs, err := startOnce()
	if err == nil {
		t.Fatalf("mesh node start by a non-admin's machine succeeded:\n%s", logs)
	}
	t.Log(err)
	if !strings.Contains(err.Error(), `"anythink"`) {
		t.Errorf("error = %q, want it to name the enterprise anythink", err)
	}
	for _, c := range f.Calls {
		if c.Name == "registerNode" {
			t.Errorf("registerNode sent for a non-admin: %v", c.Input)
		}
	}
	if y := readNodeYAML(t, home); y["node_id"] != "" || y["enterprise_id"] != "" {
		t.Errorf("node.yaml node_id %q enterprise_id %q, want both empty", y["node_id"], y["enterprise_id"])
	}
	assertNoSignIn(t, f)
}
