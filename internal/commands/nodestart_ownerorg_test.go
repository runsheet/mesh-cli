package commands

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

const grapevineOrg = "9a9a9a9a-3333-4333-8333-9a9a9a9a9a9a"

// ownerOrgFake is a platform with a-node-is-owned-by-an-org: the
// machine token reaches the grapevine org, and an org-owned
// registerNode is answered by the fake's own org answer.
func ownerOrgFake(t *testing.T, access string) *gatewaytest.Fake {
	t.Helper()
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	f.Token = access
	f.TakesNodeCredential = true
	f.NodeCredentialID = credMachine
	f.Orgs = []map[string]any{{"id": grapevineOrg, "login": "grapevine", "name": "Grapevine"}}
	f.Answer = nil
	return f
}

// ownerOrgHome is a HOME whose node.yaml says owner: org, org: grapevine.
func ownerOrgHome(t *testing.T, f *gatewaytest.Fake, token string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("MESH_GATEWAY", f.URL())
	t.Setenv("MESH_TOKEN", token)
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte("slug: mini\nname: mini\nowner: org\norg: grapevine\nsandbox: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unit, mode, once := hostUnit, nodeModeFlag, nodeOnceFlag
	hostUnit = func() sessionunit.Unit { return sessionunit.Detached{} }
	nodeModeFlag, nodeOnceFlag = "poll", true
	t.Cleanup(func() { hostUnit, nodeModeFlag, nodeOnceFlag = unit, mode, once })
	return home
}

// startOnce runs `mesh node start --mode poll --once` and returns its
// error and node.log.
func startOnce() (string, error) {
	var logs bytes.Buffer
	err := nodeStart(&cobra.Command{}, nil, log.New(&logs, "", 0))
	return logs.String(), err
}

func readNodeYAML(t *testing.T, home string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".mesh", "node.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// owner: org on a machine token: the first start registers the org as
// the owner, its id from the token's orgs read and never signIn, with
// the machine as credentialId; node.yaml keeps node_id, node_id_source
// and org_id, and a second start registers on the cached org_id.
func TestNodeStartOwnerOrgRegistersOnAMachineToken(t *testing.T) {
	access := machineJWT("machine:" + credMachine)
	f := ownerOrgFake(t, access)
	home := ownerOrgHome(t, f, machineToken(t, access, credMachine))
	logs, err := startOnce()
	if err != nil {
		t.Fatalf("mesh node start: %v\n%s", err, logs)
	}
	ins := registerInputs(t, f)
	want := gateway.NodeOwnerID("org", uuid.MustParse(grapevineOrg), "mini").String()
	for _, in := range ins {
		if in["ownerKind"] != "org" || in["ownerId"] != grapevineOrg || in[gateway.NodeCredentialField] != credMachine {
			t.Errorf("registerNode = ownerKind %v ownerId %v credentialId %v, want org %s %s", in["ownerKind"], in["ownerId"], in[gateway.NodeCredentialField], grapevineOrg, credMachine)
		}
		if in["aggregateId"] != want {
			t.Errorf("registerNode aggregateId = %v, want %s", in["aggregateId"], want)
		}
	}
	y := readNodeYAML(t, home)
	if y["node_id"] != want || y["node_id_source"] != "derived" || y["org_id"] != grapevineOrg {
		t.Errorf("node.yaml node_id %q node_id_source %q org_id %q, want %s derived %s", y["node_id"], y["node_id_source"], y["org_id"], want, grapevineOrg)
	}
	if f.SignInCalls != 0 {
		t.Errorf("signIn called %d times, want 0", f.SignInCalls)
	}
	if line := "owned by Grapevine (org), serving grapevine"; !strings.Contains(logs, line) {
		t.Errorf("node.log lacks %q:\n%s", line, logs)
	}

	// the org renamed: only the cached org_id can find it now
	f.Orgs[0]["login"] = "renamed"
	before := len(ins)
	logs, err = startOnce()
	if err != nil {
		t.Fatalf("second mesh node start: %v\n%s", err, logs)
	}
	ins = registerInputs(t, f)
	if len(ins) == before {
		t.Fatal("the second start sent no registerNode")
	}
	for _, in := range ins[before:] {
		if in["ownerId"] != grapevineOrg || in["aggregateId"] != want {
			t.Errorf("second start's registerNode ownerId %v nodeId %v, want %s %s", in["ownerId"], in["aggregateId"], grapevineOrg, want)
		}
	}
	if f.SignInCalls != 0 {
		t.Errorf("signIn called %d times across two starts, want 0", f.SignInCalls)
	}
}

// A gateway whose ownerKind enum lacks org refuses the start with one
// line naming the gateway it needs, before any registerNode.
func TestNodeStartOwnerOrgRefusedOnAGatewayWithoutIt(t *testing.T) {
	access := machineJWT("machine:" + credMachine)
	f := ownerOrgFake(t, access)
	f.OwnerKinds = []string{"person", "enterprise"}
	ownerOrgHome(t, f, machineToken(t, access, credMachine))
	logs, err := startOnce()
	if err == nil {
		t.Fatalf("mesh node start on a gateway without ownerKind org succeeded:\n%s", logs)
	}
	out := logs + err.Error() + "\n"
	if n := strings.Count(out, gateway.OwnerOrgGateway); n != 1 {
		t.Errorf("%d lines name %q, want 1:\n%s", n, gateway.OwnerOrgGateway, out)
	}
	if msg := err.Error(); strings.Contains(msg, "\n") || !strings.Contains(msg, "owner: org needs a gateway that takes ownerKind org ("+gateway.OwnerOrgGateway+"); this gateway does not") {
		t.Errorf("error = %q", msg)
	}
	for _, c := range f.Calls {
		if c.Name == "registerNode" {
			t.Fatalf("registerNode sent on a gateway without ownerKind org: %v", c.Input)
		}
	}
	if f.SignInCalls != 0 {
		t.Errorf("signIn called %d times, want 0", f.SignInCalls)
	}
}
