package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/node"
)

// A stand-in claude: announces a session, then reads stdin for as long
// as it is open, appending every line to $MESH_TEST_OUT.
const fakeClaude = `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-fake"}'
echo '{"type":"result","subtype":"success","total_cost_usd":0.01,"num_turns":1,"result":"Standing by."}'
while IFS= read -r line; do printf '%s\n' "$line" >> "$MESH_TEST_OUT"; done
exit 0
`

func TestSessionStaysAliveAndReceivesEventsOnStdin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // claudeconf writes ~/.claude.json
	out := filepath.Join(dir, "stdin.log")
	t.Setenv("MESH_TEST_OUT", out)
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 4)
	var sessionID string
	turns := make(chan string, 4)
	s := &Session{Cfg: &node.Config{Slug: "n"}, Orgs: []string{"acme"}, WorkDir: filepath.Join(dir, "work", "n"), Exe: bin, SockPath: filepath.Join(dir, "node.sock"), Binary: bin,
		OnStart: func() { started <- struct{}{} }, OnSessionID: func(id string) { sessionID = id },
		OnTurn: func(item string, cost float64) { turns <- fmt.Sprintf("%q $%.2f", item, cost) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, filepath.Join(dir, "work")) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not start")
	}
	// the idle prompt's turn ends with the fake's result: idle until the next message
	waitFor(t, func() bool { idle, _ := s.Idle(); return idle })
	select {
	case got := <-turns:
		if got != `"" $0.01` {
			t.Fatalf("turn: %s", got)
		}
	default:
		t.Fatal("the idle prompt's turn must be heard")
	}
	content, meta := ReadyEvent(&node.Resolved{Item: itemFixture(), Goal: goalFixture(), Repo: repoFixture()}, "/w/ship-it/1")
	if err := s.SendFor(meta["item_id"], UserMessage(content, meta)); err != nil {
		t.Fatal(err)
	}
	if idle, _ := s.Idle(); idle {
		t.Fatal("a sent event is a turn in flight: not idle")
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(out)
		return strings.Count(string(b), "\n") >= 2
	})
	b, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var first, second map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if first["type"] != "user" || first["message"].(map[string]any)["content"] != IdlePrompt {
		t.Fatalf("first stdin message: %s", lines[0])
	}
	got := second["message"].(map[string]any)["content"].(string)
	if !strings.HasPrefix(got, "[mesh event kind=work_item.ready item_id="+item1+" seq=1 goal_slug=ship-it repo=acme/widgets base_ref=main branch=mesh/ship-it-1 workspace=/w/ship-it/1]\n") || !strings.Contains(got, "Write the failing test") {
		t.Fatalf("event on stdin:\n%s", got)
	}
	// the result event is a turn's end, not the session's: still alive, not restarted
	time.Sleep(300 * time.Millisecond)
	if !s.Alive() || s.Starts() != 1 || sessionID != "s-fake" {
		t.Fatalf("alive=%v starts=%d session=%q", s.Alive(), s.Starts(), sessionID)
	}
	select {
	case <-started:
		t.Fatal("a live session must not be restarted")
	default:
	}
	// killed: the process ends, Run restarts it, OnStart fires again
	s.Kill("test")
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("a killed session must be restarted")
	}
	if s.Starts() != 2 {
		t.Fatalf("starts = %d after a kill", s.Starts())
	}
	waitFor(t, func() bool { return s.Alive() })
	cancel()
	select {
	case err := <-runErr:
		if err != context.Canceled {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	if s.Alive() {
		t.Fatal("stdin must close on stop")
	}
	if idle, _ := s.Idle(); idle {
		t.Fatal("a stopped session is not idle, it is gone")
	}
	args := strings.Join(s.Args("/w"), " ")
	if !strings.HasPrefix(args, "-p --input-format stream-json --output-format stream-json --verbose ") {
		t.Fatalf("args: %s", args)
	}
}

// A session restarted after its token was refreshed must carry the new
// bearer: MCPFunc is asked at every start, not once at construction.
func TestArgsAskMCPFuncEachTime(t *testing.T) {
	n := 0
	s := &Session{Cfg: &node.Config{Slug: "mini"}, MCPFunc: func() map[string]any {
		n++
		return map[string]any{"mesh-context": map[string]any{"type": "http", "url": "https://api.test/mcp",
			"headers": map[string]string{"Authorization": fmt.Sprintf("Bearer access-%d", n)}}}
	}}
	first := strings.Join(s.Args("/w"), " ")
	second := strings.Join(s.Args("/w"), " ")
	if !strings.Contains(first, "access-1") {
		t.Fatalf("first args: %s", first)
	}
	if !strings.Contains(second, "access-2") {
		t.Fatalf("second args carry a stale bearer: %s", second)
	}
}

// A stand-in claude that records its environment and what is in its
// GH_CONFIG_DIR, then waits on stdin like the real one.
const envClaude = `#!/bin/sh
env > "$MESH_TEST_OUT.env"
ls -A "$GH_CONFIG_DIR" > "$MESH_TEST_OUT.ghdir" 2>&1
echo '{"type":"system","subtype":"init","session_id":"s-env"}'
while IFS= read -r line; do :; done
exit 0
`

// The shared session is started with a fresh, empty GH_CONFIG_DIR and
// none of the GitHub tokens the node daemon's own environment carries,
// so it can never reach the operator's gh login.
func TestSessionRunsWithoutTheOperatorsGitHub(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	out := filepath.Join(dir, "claude")
	t.Setenv("MESH_TEST_OUT", out)
	operatorGH := filepath.Join(dir, "operator-gh")
	os.MkdirAll(operatorGH, 0o755)
	os.WriteFile(filepath.Join(operatorGH, "hosts.yml"), []byte("github.com:\n  oauth_token: gho_operator\n"), 0o600)
	t.Setenv("GH_CONFIG_DIR", operatorGH)
	t.Setenv("GH_TOKEN", "gho_operator")
	t.Setenv("GITHUB_TOKEN", "ghp_operator")
	t.Setenv("GH_ENTERPRISE_TOKEN", "ghe_operator")
	bin := filepath.Join(dir, "claude-bin")
	if err := os.WriteFile(bin, []byte(envClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	s := &Session{Cfg: &node.Config{Slug: "n"}, Orgs: []string{"acme"}, WorkDir: filepath.Join(dir, "work", "n"), Exe: bin, SockPath: filepath.Join(dir, "node.sock"), Binary: bin,
		OnStart: func() { started <- struct{}{} }}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, filepath.Join(dir, "work")) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not start")
	}
	var env []string
	waitFor(t, func() bool {
		e, _ := os.ReadFile(out + ".env")
		env = strings.Split(strings.TrimSpace(string(e)), "\n")
		return len(e) > 0 && fileExists(out+".ghdir")
	})
	ghDir := ""
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
			t.Errorf("the session inherited %s", kv)
		case "GH_CONFIG_DIR":
			ghDir = v
		}
		if strings.Contains(v, "_operator") {
			t.Errorf("the operator's credential reached the session: %s", kv)
		}
	}
	if ghDir == "" || ghDir == operatorGH {
		t.Fatalf("GH_CONFIG_DIR = %q", ghDir)
	}
	if b, _ := os.ReadFile(out + ".ghdir"); strings.TrimSpace(string(b)) != "" {
		t.Fatalf("GH_CONFIG_DIR must start empty, has: %s", b)
	}
	if strings.HasPrefix(ghDir, filepath.Join(dir, "work")) {
		t.Fatalf("GH_CONFIG_DIR %s is in the work root", ghDir)
	}
	cancel()
	select {
	case <-runErr:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	if _, err := os.Stat(ghDir); !os.IsNotExist(err) {
		t.Fatalf("GH_CONFIG_DIR %s outlived the session: %v", ghDir, err)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// The session is told the node has prepared and credentialed the
// workspace, and to fail the item rather than clone it for itself.
func TestSystemPromptLeavesTheCloneToTheNode(t *testing.T) {
	p := SystemPrompt(&node.Config{Slug: "n"}, []string{"acme"}, "/w")
	if strings.Contains(p, "gh repo clone") {
		t.Fatalf("the prompt still tells the session to clone:\n%s", p)
	}
	for _, want := range []string{"The node has already cloned it", "never clone", "call mesh_fail(item_id, error)"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
}

// A channel session checks main before building and may finish an item
// already done with evidence instead (adr-003 decision 2).
func TestSystemPromptChecksMainFirst(t *testing.T) {
	p := SystemPrompt(&node.Config{Slug: "n"}, []string{"acme"}, "/w")
	for _, want := range []string{
		"3. Check main first, before writing any code",
		`"gh pr list --state merged --search"`,
		"call mesh_finish(item_id, outcome=already_done, evidence)",
		"4. Do the item as a fresh subagent",
		"7. Push the branch",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
}

// The channel session feeds the node's limits too: each rate_limit_event
// on its stream is handed to OnRateLimit.
func TestSessionHandsOnItsRateLimit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("MESH_TEST_OUT", filepath.Join(dir, "stdin.log"))
	bin := filepath.Join(dir, "claude")
	script := strings.Replace(fakeClaude, "echo '{\"type\":\"result\"",
		`echo '{"type":"rate_limit_event","session_id":"s-fake","rate_limit_info":{"status":"allowed","resetsAt":1790330400,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.83,"resetsAt":1790330400},"seven_day":{"utilization":0.41,"resetsAt":1790330400}}}}'
echo '{"type":"result"`, 1)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got := make(chan *harness.RateLimit, 1)
	s := &Session{Cfg: &node.Config{Slug: "n"}, Orgs: []string{"acme"}, WorkDir: filepath.Join(dir, "work", "n"), Exe: bin, SockPath: filepath.Join(dir, "node.sock"), Binary: bin,
		OnRateLimit: func(rl *harness.RateLimit) { got <- rl }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, filepath.Join(dir, "work")) }()
	// The session writes under dir until Run returns; wait for it before
	// the TempDir is removed.
	defer func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(10 * time.Second):
			t.Error("run did not stop")
		}
	}()
	select {
	case rl := <-got:
		if rl.Status != "allowed" || rl.FiveHour.Utilization != 0.83 || rl.SevenDay.Utilization != 0.41 {
			t.Fatalf("rate limit = %+v", rl)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rate_limit_event was not handed on")
	}
}
