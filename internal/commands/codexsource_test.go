package commands

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// codexLines are the start log's lines about codex.
func codexLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(l, "codex: ") {
			lines = append(lines, l)
		}
	}
	return lines
}

// startCodex decides and builds as `mesh node start` does, on PATH as
// the test set it, and returns what was built and the log.
func startCodex(t *testing.T, sb *sandbox.Sandbox) (secondOpinion, []string) {
	t.Helper()
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	src := codexSourceOf(context.Background(), &node.Config{}, sb, exec.LookPath)
	second := codexSecondOpinionFrom(src, &node.Loop{}, t.TempDir(), logger)
	return second, codexLines(buf.String())
}

func imageWithCodex() *sandbox.Sandbox {
	return &sandbox.Sandbox{Runtime: &sandbox.Fake{Image: func(_ string, cmd []string) (string, error) {
		if strings.Join(cmd, " ") == "codex --version" {
			return "codex-cli 0.157.0", nil
		}
		return "", os.ErrNotExist
	}}, Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:v9.9.9"}}
}

// A sandboxed node with no codex on the host builds its Codex evaluator
// from the node image, Sandbox set, and says so once.
func TestCodexFromTheNodeImage(t *testing.T) {
	t.Setenv("PATH", "")
	sb := imageWithCodex()
	second, lines := startCodex(t, sb)
	if second.codex == nil || second.h == nil || second.preflight == nil {
		t.Fatal("no Codex evaluator built from the node image")
	}
	if second.codex.Sandbox != sb {
		t.Fatalf("Codex.Sandbox = %p; want the node's sandbox %p", second.codex.Sandbox, sb)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "from the node image (codex 0.157.0)") {
		t.Fatalf("codex lines %q; want one naming the node image", lines)
	}
}

// Neither the image nor the host has codex: no Codex evaluator, and one
// line saying neither.
func TestCodexFromNeither(t *testing.T) {
	t.Setenv("PATH", "")
	sb := &sandbox.Sandbox{Runtime: &sandbox.Fake{}, Config: sandbox.Config{Image: "img"}}
	second, lines := startCodex(t, sb)
	if second.codex != nil || second.h != nil {
		t.Fatal("a Codex evaluator built with no codex anywhere")
	}
	if second.blind == nil || second.noVerdict == nil || second.left == nil {
		t.Fatal("no Claude evaluation's once-per records without codex")
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "codex: no Codex evaluations: neither the node image nor the host has codex") {
		t.Fatalf("codex lines %q; want one saying neither", lines)
	}
}

// With the sandbox off, the host's codex on PATH is built from, and the
// line names it.
func TestCodexFromTheHostBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho codex-cli 0.1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	second, lines := startCodex(t, nil)
	if second.codex == nil {
		t.Fatal("no Codex evaluator built from the host binary")
	}
	if second.codex.Sandbox != nil || second.codex.Binary != bin {
		t.Fatalf("Codex Binary %q Sandbox %p; want %q on the host", second.codex.Binary, second.codex.Sandbox, bin)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "from the host binary "+bin) {
		t.Fatalf("codex lines %q; want one naming the host binary", lines)
	}
}

// A sandboxed node whose image has no codex runs none on the host, even
// when the host has one: it would run outside the pod.
func TestCodexSandboxedNeverFromTheHost(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	second, lines := startCodex(t, &sandbox.Sandbox{Runtime: &sandbox.Fake{}, Config: sandbox.Config{Image: "img"}})
	if second.codex != nil {
		t.Fatal("a sandboxed node built its Codex evaluator from the host binary")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "the node image has no codex") {
		t.Fatalf("codex lines %q", lines)
	}
}

// The sandbox off and no codex on the host: none, said so.
func TestCodexSandboxOffNoHost(t *testing.T) {
	t.Setenv("PATH", "")
	second, lines := startCodex(t, nil)
	if second.codex != nil {
		t.Fatal("built with no codex")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "the sandbox is off and the host has no codex") {
		t.Fatalf("codex lines %q", lines)
	}
}
