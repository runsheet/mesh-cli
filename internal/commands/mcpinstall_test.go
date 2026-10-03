package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshmcp"
)

// mesh mcp install writes the embedded /mesh skill to Claude Code's
// skills directory (ADR-023, Skill): $CLAUDE_CONFIG_DIR/skills/mesh/SKILL.md
// when set, else ~/.claude/skills/mesh/SKILL.md, refusing to overwrite a
// file that differs unless --force.

func mustInstall(t *testing.T, force bool) error {
	t.Helper()
	mcpInstallForce = force
	t.Cleanup(func() { mcpInstallForce = false })
	return mcpInstallCmd.RunE(mcpInstallCmd, nil)
}

func TestMcpInstallWritesTheEmbeddedSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	if err := mustInstall(t, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "skills", "mesh", "SKILL.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, meshmcp.Skill()) {
		t.Fatal("installed skill is not the embedded skill")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644", st.Mode().Perm())
	}
}

func TestMcpInstallHonoursClaudeConfigDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)

	if err := mustInstall(t, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfgDir, "skills", "mesh", "SKILL.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, meshmcp.Skill()) {
		t.Fatal("installed skill is not the embedded skill")
	}
}

func TestMcpInstallRefusesToOverwriteADifferingFileWithoutForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	path := filepath.Join(home, ".claude", "skills", "mesh", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("a person's own edits"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mustInstall(t, false); err == nil {
		t.Fatal("install over a differing file with no --force: want an error, got none")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a person's own edits" {
		t.Fatal("install with no --force changed the file")
	}

	if err := mustInstall(t, true); err != nil {
		t.Fatalf("install --force: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, meshmcp.Skill()) {
		t.Fatal("install --force did not overwrite with the embedded skill")
	}
}
