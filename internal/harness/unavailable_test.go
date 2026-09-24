package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude writes a claude that runs script (sh) and returns its path.
func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// A session the account cannot pay for is the node's condition: Run
// says so with an UnavailableError, and an ordinary failure stays one.
func TestClaudeCodeRunUnavailable(t *testing.T) {
	const spend = "You've hit your monthly spend limit · raise it at claude.ai/settings/usage"
	cases := []struct {
		name        string
		script      string
		unavailable bool
		reason      string
	}{
		{"spend limit on stderr", "echo \"" + spend + "\" >&2\nexit 1\n", true, spend},
		{"rate limit on stderr", "echo 'API Error: Rate limit reached for requests' >&2\nexit 1\n", true, "Rate limit reached"},
		{"usage limit on stderr", "echo 'Claude usage limit reached. Your limit will reset at 5pm.' >&2\nexit 1\n", true, "usage limit reached"},
		{"spend limit on stdout", "echo \"" + spend + "\"\nexit 1\n", true, spend},
		{"ordinary failure", "echo 'Error: something broke' >&2\nexit 1\n", false, ""},
		{"rate limit in a session's own text is not the harness's", `echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[{"type":"text","text":"added a rate limit"}]}}'
echo 'rate limit words on stdout'
exit 1
`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ClaudeCode{Binary: fakeClaude(t, tc.script)}
			_, err := h.Run(context.Background(), Job{Kind: KindBuild, Workspace: t.TempDir(), Prompt: "go"}, nil)
			if err == nil && tc.unavailable {
				t.Fatal("want an error")
			}
			u, ok := AsUnavailable(err)
			if ok != tc.unavailable {
				t.Fatalf("AsUnavailable = %v, want %v (err %v)", ok, tc.unavailable, err)
			}
			if ok && !strings.Contains(u.Reason, tc.reason) {
				t.Fatalf("Reason = %q, want it to contain %q", u.Reason, tc.reason)
			}
		})
	}
}

// A session with no output at all says so on its Result, whether or not
// the exit carried a known message.
func TestClaudeCodeRunEmptyOutput(t *testing.T) {
	cases := []struct {
		name   string
		script string
		empty  bool
	}{
		{"spend limit, nothing on stdout", "echo \"You've hit your monthly spend limit\" >&2\nexit 1\n", true},
		{"silent failure", "exit 1\n", true},
		{"a result", `echo '{"type":"result","subtype":"success","session_id":"s1","result":"done","total_cost_usd":0.1}'` + "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ClaudeCode{Binary: fakeClaude(t, tc.script)}
			res, _ := h.Run(context.Background(), Job{Kind: KindBuild, Workspace: t.TempDir(), Prompt: "go"}, nil)
			if res.EmptyOutput != tc.empty {
				t.Fatalf("EmptyOutput = %v, want %v", res.EmptyOutput, tc.empty)
			}
		})
	}
}

func TestParseStreamEmptyOutput(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		empty  bool
	}{
		{"nothing", "", true},
		{"only diagnostics", "You've hit your monthly spend limit\nnot json {\n", true},
		{"a result", `{"type":"result","subtype":"success","session_id":"s1","result":"done"}` + "\n", false},
		{"only an init", `{"type":"system","subtype":"init","session_id":"s1"}` + "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseStream(strings.NewReader(tc.stream), nil).EmptyOutput; got != tc.empty {
				t.Fatalf("EmptyOutput = %v, want %v", got, tc.empty)
			}
		})
	}
}

func TestAsUnavailableWrapped(t *testing.T) {
	if _, ok := AsUnavailable(nil); ok {
		t.Fatal("nil is not unavailable")
	}
	err := &UnavailableError{Reason: "rate limit"}
	if u, ok := AsUnavailable(fmt.Errorf("build: %w", err)); !ok || u.Reason != "rate limit" {
		t.Fatalf("wrapped: %v %v", u, ok)
	}
}
