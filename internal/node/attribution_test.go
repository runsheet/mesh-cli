package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// settingsOf is the --settings JSON in a session's args.
func settingsOf(t *testing.T, args []string) map[string]any {
	t.Helper()
	for i, a := range args {
		if a == "--settings" && i+1 < len(args) {
			var s map[string]any
			if err := json.Unmarshal([]byte(args[i+1]), &s); err != nil {
				t.Fatalf("--settings %q: %v", args[i+1], err)
			}
			return s
		}
	}
	t.Fatalf("no --settings in %q", args)
	return nil
}

// A build session's settings hide Claude Code's own attribution — no
// `Co-Authored-By: Claude` trailer, no PR text, no session link — on the
// host and in the sandbox alike; the host one keeps its tool-trim hook.
// The only Co-Authored-By a commit carries is the pairer's (ADR-024).
func TestSessionSettingsCarryNoCoAuthor(t *testing.T) {
	for _, sandboxed := range []bool{false, true} {
		h := &fakeHarness{}
		r, _, item, _ := runnerOn(t, h, nil)
		r.MeshBinary = "mesh"
		if sandboxed {
			r.Sandbox = &sandbox.Sandbox{Runtime: &sandbox.Fake{}, Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:v1"}, TempDir: t.TempDir()}
		}
		if err := r.RunItem(context.Background(), item); err != nil {
			t.Fatal(err)
		}
		if len(h.jobs) != 1 {
			t.Fatalf("sandboxed %v: %d sessions", sandboxed, len(h.jobs))
		}
		job := h.jobs[0]
		cc := &harness.ClaudeCode{}
		args := cc.Args(job)
		if sandboxed {
			if job.Session == nil {
				t.Fatal("the sandboxed build ran in no pod")
			}
			args = cc.SandboxArgs(job, "/home/mesh/mcp.json")
		}
		s := settingsOf(t, args)
		at, _ := s["attribution"].(map[string]any)
		if at == nil || at["commit"] != "" || at["pr"] != "" || at["sessionUrl"] != false {
			t.Errorf("sandboxed %v: attribution = %v, want commit \"\", pr \"\", sessionUrl false", sandboxed, s["attribution"])
		}
		hooks := bashHooks(t, args)
		if !sandboxed && (len(hooks) != 1 || !strings.Contains(hooks[0], "mesh tool-trim --max-kb 16")) {
			t.Errorf("host build: Bash hooks %v, want the tool-trim hook", hooks)
		}
		if sandboxed && len(hooks) != 0 {
			t.Errorf("sandboxed build: Bash hooks %v, want none (no mesh binary in the pod)", hooks)
		}
	}
}

// The PR body a finish carries names who built and paired, and never
// Claude Code's attribution.
func TestPullRequestBodyCarriesNoClaudeAttribution(t *testing.T) {
	for _, pairers := range [][]Pairer{nil, {{Name: "Ada Lovelace", Email: "ada@example.com"}}} {
		body := FinishDescription(FinishWho{Actor: "agent:builder@mini", Model: "opus", Adapter: "claude-code", Pairers: pairers})
		lower := strings.ToLower(body)
		if strings.Contains(lower, "co-authored-by: claude") || strings.Contains(body, "Generated with") {
			t.Errorf("pairers %v: body carries Claude's attribution:\n%s", pairers, body)
		}
		for _, p := range pairers {
			if !strings.Contains(body, p.Name) {
				t.Errorf("body names no %s:\n%s", p.Name, body)
			}
		}
	}
}
