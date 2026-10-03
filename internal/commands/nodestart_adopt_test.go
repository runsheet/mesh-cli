package commands

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// The grapevine node's v0.2.248 start, replayed (goal
// a-node-takes-the-id-the-platform-names): node.yaml carries the id the
// node derived, the platform registers it only at the id it derives
// (Fake.RegistersAt) and refuses the other naming it. `mesh node
// start`'s registration adopts it, logs the adoption after the start
// line and registers again under it — two registerNode calls — and
// node.yaml keeps the adopted id.
func TestNodeStartAdoptsTheIDTheRefusalNames(t *testing.T) {
	const (
		sent    = "47a08dd9-bafb-5c64-bb16-7e806f0b1b95"
		adopted = "18a8b766-18f3-5e73-b4cf-efb91c01946a"
		org     = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	t.Setenv("MESH_GATEWAY", f.URL())
	t.Setenv("MESH_TOKEN", "rs_test")
	f.UserID = "ffffffff-1111-4111-8111-ffffffffffff"
	f.Orgs = []map[string]any{{"id": org, "tenantSlug": "grapevine"}}
	f.RegistersAt = adopted
	f.Answer = &gateway.NodeAnswer{NodeID: adopted, Owner: gateway.NodeOwner{Kind: "person", ID: f.UserID, Name: "Chris"},
		Scopes: []gateway.ScopeAnswer{{OrgID: org, Slug: "grapevine"}}}
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "slug: grapevine\nname: grapevine\nowner_id: " + f.UserID + "\nnode_id: " + sent + "\ncontract: owner\nsandbox: off\n"
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	state := &selfupdate.StateFile{Path: filepath.Join(t.TempDir(), "update.json")}
	_, _, reg, _, err := registerNode(context.Background(), &cobra.Command{}, log.New(&logs, "", 0), state, node.StartCheck{}, time.Now())
	if err != nil {
		t.Fatalf("registerNode: %v\n%s", err, logs.String())
	}

	var regs []string
	for _, c := range f.Calls {
		if c.Name == "registerNode" {
			id, _ := c.Input["aggregateId"].(string)
			regs = append(regs, id)
		}
	}
	if len(regs) != 2 || regs[0] != sent || regs[1] != f.RegistersAt {
		t.Fatalf("registerNode calls at %q, want %s then %s", regs, sent, f.RegistersAt)
	}
	want := "owner id adopted from the platform: " + adopted + " (derived " + sent + ")"
	if !strings.Contains(logs.String(), want+"\n") {
		t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
	}
	if reg.Owned == nil || reg.Owned.NodeID != adopted {
		t.Fatalf("registered %+v, want owned at %s", reg.Owned, adopted)
	}
	saved, err := node.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.NodeID != adopted || saved.NodeIDSource != node.IDAdopted {
		t.Fatalf("node.yaml node_id %q node_id_source %q", saved.NodeID, saved.NodeIDSource)
	}
}
