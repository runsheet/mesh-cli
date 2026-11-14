package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mesh node model is message-only on a v1 node (ADR-044): a lease runs
// on the model its job brief names, so there is no node.yaml model
// table left to show or set, and the command writes nothing there.
func TestNodeModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var buf bytes.Buffer
	nodeModelCmd.SetOut(&buf)
	t.Cleanup(func() { nodeModelCmd.SetOut(nil) })
	if err := nodeModelCmd.RunE(nodeModelCmd, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"job brief", "node.yaml"} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".mesh", "node.yaml")); !os.IsNotExist(err) {
		t.Fatalf("mesh node model touched node.yaml: stat err = %v", err)
	}
}
