package nodev1

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
)

// fakeClaude is a claude that writes its args and stdin beside itself
// and prints a stream-json session.
const fakeClaude = `#!/bin/sh
dir=$(dirname "$0")
printf '%s\n' "$@" > "$dir/args"
cat > "$dir/stdin"
echo '{"type":"system","subtype":"init","session_id":"s-77"}'
echo '{"type":"result","subtype":"success","is_error":'"$FAKE_IS_ERROR"',"result":"all done","session_id":"s-77","total_cost_usd":0.05,"num_turns":4}'
exit ${FAKE_EXIT:-0}
`

func writeFakeClaude(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for claude")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// A session is `claude -p` stream-json, the prompt on stdin, the model
// and budget the brief's, every server proxied, no attribution; its end
// is read from the stream.
func TestClaudeSession(t *testing.T) {
	bin := writeFakeClaude(t)
	c := &ClaudeSession{Binary: bin, ExtraArgs: []string{"--dangerously-skip-permissions"}}
	spec := SessionSpec{
		Workspace: t.TempDir(), Prompt: "do the job", Model: "haiku", BudgetUSD: 1.5,
		Servers: map[string]any{"conform": harness.ProxyRelay("/bin/mesh", "/run/s", "conform")},
		Env:     []string{"FAKE_IS_ERROR=false"},
	}
	res, err := c.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "s-77" || res.Text != "all done" || res.CostUSD != 0.05 || res.Turns != 4 {
		t.Fatalf("%+v", res)
	}
	args, _ := os.ReadFile(filepath.Join(filepath.Dir(bin), "args"))
	stdin, _ := os.ReadFile(filepath.Join(filepath.Dir(bin), "stdin"))
	a := string(args)
	for _, want := range []string{"-p\n", "stream-json", "--model\nhaiku", "--max-budget-usd\n1.50", "--mcp-config", "mcp-relay", "mcp__conform__*", `"attribution"`, "--dangerously-skip-permissions"} {
		if !strings.Contains(a, want) {
			t.Errorf("args lack %q:\n%s", want, a)
		}
	}
	if string(stdin) != "do the job" || strings.Contains(a, "do the job") {
		t.Fatalf("the prompt is not on stdin alone: %q", stdin)
	}
}

// A session whose stream ends in an error, or whose process fails, is
// an error carrying the reason.
func TestClaudeSessionErrors(t *testing.T) {
	bin := writeFakeClaude(t)
	c := &ClaudeSession{Binary: bin}
	_, err := c.Run(context.Background(), SessionSpec{Workspace: t.TempDir(), Env: []string{"FAKE_IS_ERROR=true"}})
	if err == nil || !strings.Contains(err.Error(), "all done") {
		t.Fatalf("an error result: %v", err)
	}
	_, err = c.Run(context.Background(), SessionSpec{Workspace: t.TempDir(), Env: []string{"FAKE_IS_ERROR=false", "FAKE_EXIT=3"}})
	if err == nil {
		t.Fatal("a failed process was no error")
	}
}
