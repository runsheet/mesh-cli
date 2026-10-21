package commands

import (
	"os"
	"testing"

	"projectmesh.io/mesh-cli/internal/node"
)

// A session's text goes to sessions.log and the node's own line to
// node.log: node.log is the node's reasoning alone.
func TestSessionTextLandsApartFromNodeLines(t *testing.T) {
	home := t.TempDir()
	logs, err := node.OpenNodeLogs(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs.Node.SetFlags(0)
	logs.Sessions.SetFlags(0)
	logs.Node.Printf("claimed item node-yaml/4")
	logs.Sessions.Printf("session: %s", firstLine("Reading the brief first.\nThen the code."))
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	nodeLog, _ := os.ReadFile(node.ServiceLog(home))
	sessLog, _ := os.ReadFile(node.SessionsLog(home))
	if got, want := string(nodeLog), "mesh node: claimed item node-yaml/4\n"; got != want {
		t.Fatalf("node.log = %q, want %q", got, want)
	}
	if got, want := string(sessLog), "mesh node: session: Reading the brief first. …\n"; got != want {
		t.Fatalf("sessions.log = %q, want %q", got, want)
	}
}
