package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The child sees the chosen account's credential and config dir, never
// the operator's: the unset variables are dropped before Env is added.
func TestRunUnsetsInheritedAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "operator-api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "operator-auth-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "operator-oauth")
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/operator/.claude")
	t.Setenv("MESH_KEEP_ME", "kept")
	out := filepath.Join(t.TempDir(), "env")
	bin := fakeClaude(t, "env > "+out+"\necho '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"s\",\"result\":\"ok\"}'\n")
	c := &ClaudeCode{Binary: bin}
	job := Job{
		Prompt: "hi", Workspace: t.TempDir(),
		Unset: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"},
		Env:   []string{"CLAUDE_CODE_OAUTH_TOKEN=account-oauth", "CLAUDE_CONFIG_DIR=/tmp/mesh-claude-x"},
	}
	if _, err := c.Run(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string][]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			seen[k] = append(seen[k], v)
		}
	}
	for k, want := range map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "account-oauth", "CLAUDE_CONFIG_DIR": "/tmp/mesh-claude-x", "MESH_KEEP_ME": "kept"} {
		if got := seen[k]; len(got) != 1 || got[0] != want {
			t.Errorf("child %s = %v, want only %q", k, got, want)
		}
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if got, ok := seen[k]; ok {
			t.Errorf("child sees the operator's %s = %v", k, got)
		}
	}
	if strings.Contains(string(data), "operator-") {
		t.Errorf("an operator value reached the child:\n%s", data)
	}
}

// A job with no Unset inherits everything, as before.
func TestSessionEnvironNoUnset(t *testing.T) {
	got := SessionEnviron([]string{"A=1", "B=2"}, Job{Env: []string{"C=3"}})
	if strings.Join(got, " ") != "A=1 B=2 C=3" {
		t.Fatalf("env %v", got)
	}
}

// A session that dies with the secret on its stderr: the error and the
// stderr the node logs carry it redacted.
func TestRunRedactsSecrets(t *testing.T) {
	const secret = "sk-ant-oat-SECRET"
	bin := fakeClaude(t, "echo \"auth failed for $CLAUDE_CODE_OAUTH_TOKEN\" >&2\nexit 1\n")
	var stderr bytes.Buffer
	c := &ClaudeCode{Binary: bin, Stderr: &stderr}
	job := Job{Prompt: "hi", Workspace: t.TempDir(), Env: []string{"CLAUDE_CODE_OAUTH_TOKEN=" + secret}, Secrets: []string{secret}}
	_, err := c.Run(context.Background(), job, nil)
	if err == nil {
		t.Fatal("a failing session returned no error")
	}
	for what, s := range map[string]string{"error": err.Error(), "stderr": stderr.String()} {
		if strings.Contains(s, secret) {
			t.Errorf("the secret is in the %s: %s", what, s)
		}
		if !strings.Contains(s, "auth failed for "+Redacted) {
			t.Errorf("%s %q: want it redacted", what, s)
		}
	}
}
