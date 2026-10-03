package meshmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// TestPersonalServerOverStdio drives `mesh mcp`'s server as Claude Code
// would: initialize, tools/list and a mesh_goals call over a stdin/stdout
// pair, against the fake gateway. It also asserts the personal node has
// registered by the time the first reply comes back, well before
// tools/list answers (ADR-015's owner form, ADR-023 mesh mcp).
func TestPersonalServerOverStdio(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Orgs = []map[string]any{{"id": "org-1", "login": "acme"}}
	f.Goals["goal-1"] = map[string]any{"id": "goal-1", "orgId": "org-1", "slug": "ship-it", "title": "Ship it", "status": "running"}
	f.Goals["goal-2"] = map[string]any{"id": "goal-2", "orgId": "org-1", "slug": "long-gone", "title": "Long gone", "status": "done"}
	f.WorkItems["item-1"] = map[string]any{"id": "item-1", "goalId": "goal-1", "orgId": "org-1", "seq": 1, "title": "Build the thing", "status": "ready"}
	f.WorkItems["item-2"] = map[string]any{"id": "item-2", "goalId": "goal-1", "orgId": "org-1", "seq": 2, "title": "Merged already", "status": "merged"}

	srv := NewServer(gateway.New(f.URL(), "rs_test"))
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, inR, outW) }()
	replies := bufio.NewScanner(outR)
	replies.Buffer(make([]byte, 0, 64*1024), 1<<20)

	call := func(id int, method string, params any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		if !replies.Scan() {
			t.Fatalf("%s: no reply: %v", method, replies.Err())
		}
		var msg struct {
			Result map[string]any `json:"result"`
			Error  any            `json:"error"`
		}
		if err := json.Unmarshal(replies.Bytes(), &msg); err != nil {
			t.Fatalf("%s: %v: %s", method, err, replies.Bytes())
		}
		if msg.Error != nil {
			t.Fatalf("%s: error %v", method, msg.Error)
		}
		return msg.Result
	}

	init := call(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if info := init["serverInfo"].(map[string]any); info["name"] != "mesh" {
		t.Errorf("serverInfo %v", info)
	}
	if _, ok := init["capabilities"].(map[string]any)["experimental"]; ok {
		t.Errorf("mesh mcp advertises the channel capability: %v", init["capabilities"])
	}
	// the personal node registers before Serve answers its first
	// request at all (ADR-015's owner form, ADR-023 mesh mcp)
	if calls := f.CallsNamed("registerNode"); len(calls) != 1 {
		t.Fatalf("registerNode calls by the time initialize answered = %d, want 1", len(calls))
	}
	// a notification is not answered
	_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))

	var names []string
	for _, tl := range call(2, "tools/list", map[string]any{})["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	want := []string{"mesh_goals", "mesh_status", "mesh_join", "mesh_switch", "mesh_progress", "mesh_ask", "mesh_finish", "mesh_release"}
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("tools/list names %v, want exactly %v", names, want)
	}

	text := func(res map[string]any) string {
		if res["isError"] == true {
			t.Errorf("isError: %v", res)
		}
		return res["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	goals := text(call(3, "tools/call", map[string]any{"name": "mesh_goals", "arguments": map[string]any{}}))
	if !strings.Contains(goals, "acme/ship-it") || !strings.Contains(goals, "1 Build the thing (ready)") {
		t.Errorf("mesh_goals:\n%s", goals)
	}
	if strings.Contains(goals, "long-gone") || strings.Contains(goals, "Merged already") {
		t.Errorf("mesh_goals lists an ended goal or item:\n%s", goals)
	}
	if got := text(call(4, "tools/call", map[string]any{"name": "mesh_join", "arguments": map[string]any{"goal": "ship-it"}})); got != NotYetAvailable {
		t.Errorf("mesh_join = %q", got)
	}
	if got := text(call(5, "tools/call", map[string]any{"name": "mesh_status", "arguments": map[string]any{}})); !strings.Contains(got, "holding no item") {
		t.Errorf("mesh_status = %q", got)
	}

	inW.Close()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
