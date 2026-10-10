package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
)

// The test binary is mesh itself when MESH_TEST_MAIN is set, so a test
// runs the command line as an operator — or a session — would.
func TestMain(m *testing.M) {
	if os.Getenv("MESH_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// `echo <90KB> | mesh tool-trim --max-kb 16` prints the head and tail
// with the trim note between.
func TestToolTrim(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 90*1024; i++ {
		fmt.Fprintf(&b, "=== RUN TestSomething/%05d --- PASS (0.00s)\n", i)
	}
	in := b.String()
	cmd := exec.Command("bash", "-c", `echo "$INPUT" | "$MESH" tool-trim --max-kb 16`)
	cmd.Env = append(os.Environ(), "MESH_TEST_MAIN=1", "MESH="+os.Args[0], "INPUT="+strings.TrimSuffix(in, "\n"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("tool-trim: %v", err)
	}
	got := string(out)
	if len(got) >= 17*1024 {
		t.Errorf("printed %d bytes, want under 17 KB", len(got))
	}
	if !strings.HasPrefix(got, "=== RUN TestSomething/00000 ") || !strings.Contains(got, "\n[mesh: trimmed ") {
		t.Errorf("not the trimmed text: %q", got[:200])
	}
	lines := strings.Split(strings.TrimSuffix(in, "\n"), "\n")
	if !strings.HasSuffix(got, lines[len(lines)-1]+"\n") {
		t.Errorf("does not end with the last line: %q", got[len(got)-100:])
	}
}

// `mesh node status` over a state dir holding the hour's attestation
// outcomes — two recorded and one refused inside the hour, one recorded
// older — prints their counts (goal the-node-attests-the-head-it-pushed,
// item 8).
func TestNodeStatusShowsAttestationCounts(t *testing.T) {
	const (
		org    = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
		user   = "ffffffff-1111-4111-8111-ffffffffffff"
		nodeID = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	)
	home := t.TempDir()
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	f.UserID = user
	f.Orgs = []map[string]any{{"id": org, "tenantSlug": "runsheet"}}
	f.Answer = &gateway.NodeAnswer{NodeID: nodeID, Owner: gateway.NodeOwner{Kind: "person", ID: user, Name: "Chris"},
		Scopes: []gateway.ScopeAnswer{{OrgID: org, Login: "runsheet"}}}
	gw := gateway.New(f.URL(), "rs_test")
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: nodeID, OrgID: org, OwnerID: user, Name: "mini", Slug: "runsheet-mini", Owner: "person", Capacity: 1, Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	state := node.RunState{Dir: filepath.Join(home, ".mesh")}
	now := time.Now()
	if err := state.WriteAttestations([]node.AttestationOutcome{
		{At: now.Add(-2 * time.Hour)},
		{At: now.Add(-40 * time.Minute)},
		{At: now.Add(-20 * time.Minute), Refused: true},
		{At: now.Add(-5 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	yaml := "slug: runsheet-mini\nname: mini\nowner_id: " + user + "\nnode_id: " + nodeID + "\ncontract: owner\nsandbox: off\n"
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "node", "status")
	cmd.Env = append(os.Environ(), "MESH_TEST_MAIN=1", "HOME="+home, "PATH="+t.TempDir(), "MESH_GATEWAY="+f.URL(), "MESH_TOKEN=rs_test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mesh node status: %v\n%s", err, out)
	}
	want := "attestations: 2 recorded, 1 refused (last hour)"
	for _, line := range strings.Split(string(out), "\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("no %q line:\n%s", want, out)
}
