package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/harness"
)

// buildJob runs one build with node.yaml's tool_results as yml ("" for
// none) and returns the job the session got, and its rendered args.
func buildJob(t *testing.T, yml string) (harness.Job, []string) {
	t.Helper()
	h := &fakeHarness{}
	r, _, item, _ := runnerOn(t, h, nil)
	if yml != "" {
		if err := yaml.Unmarshal([]byte(yml), r.Cfg); err != nil {
			t.Fatal(err)
		}
	}
	r.MeshBinary = "mesh"
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("%d sessions", len(h.jobs))
	}
	job := h.jobs[0]
	return job, (&harness.ClaudeCode{}).Args(job)
}

// bashHooks are the PreToolUse commands on Bash in the args' --settings.
func bashHooks(t *testing.T, args []string) []string {
	t.Helper()
	var cmds []string
	for i, a := range args {
		if a != "--settings" || i+1 == len(args) {
			continue
		}
		var s struct {
			Hooks struct {
				PreToolUse []struct {
					Matcher string `json:"matcher"`
					Hooks   []struct {
						Type    string `json:"type"`
						Command string `json:"command"`
					} `json:"hooks"`
				} `json:"PreToolUse"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal([]byte(args[i+1]), &s); err != nil {
			t.Fatalf("settings: %v", err)
		}
		for _, m := range s.Hooks.PreToolUse {
			for _, hk := range m.Hooks {
				if m.Matcher == "Bash" && hk.Type == "command" {
					cmds = append(cmds, hk.Command)
				}
			}
		}
	}
	return cmds
}

// A build session's settings carry the tool-trim hook on Bash and its
// env the CLI's output limits, both from node.yaml's tool_results: 16 KB
// by default, 32 when it says so, and neither when it says 0.
func TestBuildSessionTrimsToolResults(t *testing.T) {
	for _, c := range []struct {
		yml       string
		hook      string
		mcp, bash string
	}{
		{"", "mesh tool-trim --max-kb 16", "4096", "16384"},
		{"tool_results: {max_kb: 32}", "mesh tool-trim --max-kb 32", "8192", "32768"},
		{"tool_results: {max_kb: 0}", "", "", ""},
	} {
		job, args := buildJob(t, c.yml)
		var trims []string
		for _, h := range bashHooks(t, args) {
			if strings.Contains(h, "tool-trim") {
				trims = append(trims, h)
			}
		}
		if c.hook == "" {
			if len(trims) != 0 {
				t.Errorf("%q: trim hooks %v; want none", c.yml, trims)
			}
		} else if len(trims) != 1 || !strings.Contains(trims[0], c.hook) {
			t.Errorf("%q: Bash trim hooks %v, want one naming %q", c.yml, trims, c.hook)
		}
		for k, want := range map[string]string{"MAX_MCP_OUTPUT_TOKENS": c.mcp, "BASH_MAX_OUTPUT_LENGTH": c.bash} {
			got, ok := envOf(job.Env, k)
			if want == "" && ok {
				t.Errorf("%q: %s=%s, want unset", c.yml, k, got)
			}
			if want != "" && got != want {
				t.Errorf("%q: %s=%q, want %s", c.yml, k, got, want)
			}
		}
	}
}
