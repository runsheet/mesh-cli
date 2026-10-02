package commands

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/selfupdate"
)

const (
	credOrg   = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	credAcct  = "bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb"
	credMesh  = "cccccccc-1111-4111-8111-cccccccccccc"
	credLatch = "dddddddd-1111-4111-8111-dddddddddddd"
)

// startAgainst runs `mesh node start`'s registration against the fake
// and the loop's first ticks, whose first heartbeat makes the
// credentials report, returning node.log.
func startAgainst(t *testing.T, f *gatewaytest.Fake) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MESH_GATEWAY", f.URL())
	t.Setenv("MESH_TOKEN", "rs_test")
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte("slug: mini\nname: mini\norgs: [acme]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// the platform answers with the node's own id: the signed-in
	// owner's node of this slug
	f.UserID = "ffffffff-1111-4111-8111-ffffffffffff"
	f.Answer.NodeID = gateway.NodeID(uuid.MustParse(f.UserID), "mini").String()
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	state := &selfupdate.StateFile{Path: filepath.Join(t.TempDir(), "update.json")}
	cfg, gw, reg, _, err := registerNode(context.Background(), &cobra.Command{}, logger, state, node.StartCheck{}, time.Now())
	if err != nil {
		t.Fatalf("register: %v\n%s", err, logs.String())
	}
	loop := &node.Loop{GW: gw, Cfg: cfg, Scopes: reg.Scopes, Owned: reg.Owned, Interval: time.Hour, Harness: "claude-code", Log: logger,
		LookPath: func(string) (string, error) { return "", errors.New("not found") }}
	loop.TakeAnswer(reg.Answer)
	loop.FirstBeat = func(ctx context.Context, scopes node.Scopes) { reportCredentials(ctx, gw, loop, scopes, logger) }
	// two ticks: the report is the first heartbeat's, and once
	for range 2 {
		loop.Tick(context.Background())
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
}
