package commands

import (
	"os"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/node"
)

// `mesh node status`'s findings lines list a critical finding before a
// warn one, and by kind within a severity, and print nothing without a
// running node (goal the-quartermaster-keeps-the-nodes-fit).
func TestFindingsLinesOrder(t *testing.T) {
	got := findingsLines([]node.Finding{
		{Kind: "scratch", Severity: "warn", Subject: "/tmp/runsheet-foo", Detail: "67G"},
		{Kind: "disk", Severity: "critical", Subject: "/tmp", Detail: "3% free"},
	})
	want := []string{
		"findings:",
		"  disk critical /tmp: 3% free",
		"  scratch warn /tmp/runsheet-foo: 67G",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findingsLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// With nothing found, the block says so rather than listing nothing.
func TestFindingsLinesNone(t *testing.T) {
	got := findingsLines(nil)
	if len(got) != 1 || got[0] != "findings: none" {
		t.Fatalf("findingsLines(nil) = %+v, want [\"findings: none\"]", got)
	}
}

// localFindings only reports findings, and only true, while a node
// runs here — the same pid rule as localStanding and localHeld.
func TestLocalFindingsPidRule(t *testing.T) {
	state := node.RunState{Dir: t.TempDir()}
	if _, ok := localFindings(state); ok {
		t.Fatal("localFindings with no node running")
	}
	pid := os.Getpid()
	if err := state.Started(pid); err != nil {
		t.Fatal(err)
	}
	if findings, ok := localFindings(state); !ok || len(findings) != 0 {
		t.Fatalf("localFindings with a node running but nothing found = %+v, %v, want [], true", findings, ok)
	}
	if err := state.WriteFindings([]node.Finding{{Kind: "disk", Severity: "critical", Subject: "/tmp"}}); err != nil {
		t.Fatal(err)
	}
	if findings, ok := localFindings(state); !ok || len(findings) != 1 {
		t.Fatalf("localFindings = %+v, %v, want one entry", findings, ok)
	}
	state.Stopped(pid)
	if _, ok := localFindings(state); ok {
		t.Fatal("localFindings after the node stopped")
	}
}
