package commands

import (
	"strings"
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

// `mesh node model` shows the model the planner will actually run on —
// the strongest configured when plan is unset, opus when none is — and
// ends with how an item's own model comes before the table.
func TestNodeModelShowsPrecedence(t *testing.T) {
	for _, c := range []struct {
		models node.Models
		plan   string
	}{
		{node.Models{Default: "haiku", Build: "sonnet"}, "plan      sonnet (the strongest configured)\n"},
		{node.Models{}, "plan      opus (the strongest configured)\n"},
		{node.Models{Default: "opus", Plan: "haiku"}, "plan      haiku\n"},
	} {
		t.Setenv("HOME", t.TempDir())
		cfg := &node.Config{Name: "n", Slug: "n", Capacity: node.Fixed(1), Models: c.models}
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		out := captureStdout(func() {
			if err := nodeModelCmd.RunE(nodeModelCmd, nil); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, c.plan) {
			t.Fatalf("%+v: no %q in:\n%s", c.models, c.plan, out)
		}
		for _, line := range []string{
			"build and rebase sessions run on the item's model, then its agent's, then\n",
			"node.yaml's build (or default). The evaluator runs on the stronger of the\n",
			"builder's model and sonnet. The planner runs on plan if set, else the\n",
			"strongest model configured (opus when none is).\n",
		} {
			if !strings.Contains(out, line) {
				t.Fatalf("no %q in:\n%s", line, out)
			}
		}
	}
}
