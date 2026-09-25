package commands

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/node"
)

// The rebase kind is set like every other: --model-rebase on `node
// start` wins over the file, and `node model --rebase` saves it.
func TestModelsRebaseKind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &node.Config{Name: "n", Slug: "n", Capacity: node.Fixed(1), Models: node.Models{Default: "opus", Rebase: "haiku"}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := nodeStartCmd.Flags().Set("model-rebase", "sonnet"); err != nil {
		t.Fatal(err)
	}
	if err := nodeStartCmd.Flags().Set("model-research", "haiku"); err != nil {
		t.Fatal(err)
	}
	m := modelsFor(nodeStartCmd, cfg)
	if m.For("rebase") != "sonnet" || m.For("research") != "haiku" || m.For("build") != "opus" {
		t.Fatalf("flags: %+v", m)
	}

	if err := nodeModelCmd.Flags().Set("rebase", "claude-opus-5"); err != nil {
		t.Fatal(err)
	}
	if err := nodeModelCmd.RunE(nodeModelCmd, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := node.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Models.Rebase != "claude-opus-5" || saved.Models.For("rebase") != "claude-opus-5" {
		t.Fatalf("saved: %+v", saved.Models)
	}
}
