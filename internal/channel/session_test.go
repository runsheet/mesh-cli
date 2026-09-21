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
