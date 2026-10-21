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

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

const (
	credOrg   = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	credAcct  = "bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb"
	credMesh  = "cccccccc-1111-4111-8111-cccccccccccc"
	credLatch = "dddddddd-1111-4111-8111-dddddddddddd"
)

// startAgainst runs `mesh node start --once` — nodeStart
// itself, registration through the loop's one tick, whose first
// heartbeat makes the credentials report — against the fake, and
// returns node.log. The host's sessions are out of its reach: PATH is
// empty, so no claude or codex is found, and the start-up sweep runs
// on no systemd scope of this machine's.
func startAgainst(t *testing.T, f *gatewaytest.Fake) string {
	t.Helper()
	return startAgainstOn(t, f, "rs_test")
}

// startAgainstOn is startAgainst on token as MESH_TOKEN: an rs_* console
// token, or a machine credential's JSON.
func startAgainstOn(t *testing.T, f *gatewaytest.Fake, token string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("MESH_GATEWAY", f.URL())
	t.Setenv("MESH_TOKEN", token)
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte("slug: mini\nname: mini\norgs: [acme]\nsandbox: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// the platform answers with the node's own id: the signed-in
	// owner's node of this slug
	f.UserID = "ffffffff-1111-4111-8111-ffffffffffff"
	f.Answer.NodeID = gateway.NodeOwnerID("person", uuid.MustParse(f.UserID), "mini").String()
	unit, mode, once := hostUnit, nodeModeFlag, nodeOnceFlag
	hostUnit = func() sessionunit.Unit { return sessionunit.Detached{} }
	nodeModeFlag, nodeOnceFlag = "poll", true
	t.Cleanup(func() { hostUnit, nodeModeFlag, nodeOnceFlag = unit, mode, once })
	var logs bytes.Buffer
	if err := nodeStart(&cobra.Command{}, nil, log.New(&logs, "", 0)); err != nil {
		t.Fatalf("mesh node start: %v\n%s", err, logs.String())
	}
	return logs.String()
}

func credentialFake(t *testing.T) *gatewaytest.Fake {
	t.Helper()
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	f.Orgs = []map[string]any{{"id": credOrg, "login": "acme", "accountId": credAcct}}
	// the platform's answer to the owned node: the one org it serves
	f.Answer = &gateway.NodeAnswer{Owner: gateway.NodeOwner{Kind: "person", ID: "u-1", Name: "Chris"},
		Scopes: []gateway.ScopeAnswer{{OrgID: credOrg, Login: "acme", AccountID: credAcct}}}
	f.Accounts[credAcct] = map[string]any{"id": credAcct, "login": "acme"}
	f.Projects[credMesh] = map[string]any{"id": credMesh, "orgId": credOrg, "slug": "mesh", "name": "Mesh"}
	f.Projects[credLatch] = map[string]any{"id": credLatch, "orgId": credOrg, "slug": "latchkey", "name": "Latchkey"}
	f.Credentials = []map[string]any{{"id": "cred-1", "orgId": credOrg, "label": "max", "vendor": "anthropic", "kind": "subscription",
		"serves": []string{"claude-code"}, "projectIds": []string{credMesh}, "allowedNodeIds": []string{}, "status": "active"}}
	return f
}

// A start against a platform where one project has no claude-code
// credential open to the node names that project, once, and no other.
func TestNodeStartNamesAProjectWithNoCredential(t *testing.T) {
	logs := startAgainst(t, credentialFake(t))
	want := "credentials: org acme's project latchkey has no claude-code credential open to this node"
	if strings.Count(logs, want) != 1 {
		t.Fatalf("node.log lacks %q once:\n%s", want, logs)
	}
	if !strings.Contains(logs, "node mini (mini) for") {
		t.Fatalf("not the start's log:\n%s", logs)
	}
	if strings.Contains(logs, "project mesh has no") || strings.Contains(logs, "every project") {
		t.Fatalf("node.log names a covered project:\n%s", logs)
	}
}

// Every project covered: the one line saying so.
func TestNodeStartSaysEveryProjectIsCovered(t *testing.T) {
	f := credentialFake(t)
	f.Credentials[0]["projectIds"] = []string{credMesh, credLatch}
	logs := startAgainst(t, f)
	if !strings.Contains(logs, "credentials: every project has a claude-code credential open to this node") {
		t.Fatalf("node.log:\n%s", logs)
	}
}

// A platform that refuses credentialIndexs: the notice once, and the
// start goes on.
func TestNodeStartContinuesWhenCredentialsAreRefused(t *testing.T) {
	f := credentialFake(t)
	f.FailReads["credentialIndexs"] = "not authorized: CredentialIndex"
	logs := startAgainst(t, f)
	if n := strings.Count(logs, node.CoverageRefused); n != 1 {
		t.Fatalf("the refusal notice %d times, want once:\n%s", n, logs)
	}
	if strings.Contains(logs, "has no claude-code") {
		t.Fatalf("named projects without a read:\n%s", logs)
	}
	// and the start went on: the loop ran its tick past the notice and
	// nodeStart returned nil (startAgainst fails on its error)
	if !strings.Contains(logs, "node mini (mini) for") {
		t.Fatalf("the start did not go on:\n%s", logs)
	}
}
