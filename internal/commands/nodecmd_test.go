package commands

import (
	"bytes"
	"strings"
	"testing"
)

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	buf := &bytes.Buffer{}
	rootCmd.SetArgs(args)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	defer func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	}()
	err := rootCmd.Execute()
	return buf.String(), err
}

// The v0 node's entry points are gone: mesh node start and mesh node
// sweep are unknown commands, not a silent fall-through to node's help.
func TestNodeStartAndSweepAreUnknown(t *testing.T) {
	for _, sub := range []string{"start", "sweep"} {
		_, err := runRoot(t, "node", sub)
		if err == nil || !strings.Contains(err.Error(), `unknown command "`+sub+`" for "mesh node"`) {
			t.Errorf("mesh node %s: err = %v, want unknown command", sub, err)
		}
	}
}

func TestNodeHelpListsNoStartOrSweep(t *testing.T) {
	out, err := runRoot(t, "node", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  run ") {
		t.Fatalf("node help lists no run:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasPrefix(line, "  ") && (f[0] == "start" || f[0] == "sweep") {
			t.Errorf("node help lists %s: %q", f[0], line)
		}
	}
}

// mesh node with nothing after it still prints its help and succeeds.
func TestBareNodePrintsHelp(t *testing.T) {
	out, err := runRoot(t, "node")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Available Commands:") {
		t.Fatalf("bare node printed:\n%s", out)
	}
}
