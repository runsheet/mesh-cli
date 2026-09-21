package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
)

const (
	orgID  = "0f3b8a1e-2c4d-4e6f-8a9b-0c1d2e3f4a5b"
	acctID = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	projID = "bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb"
	repoID = "cccccccc-1111-4111-8111-cccccccccccc"
	item1  = "eeeeeeee-1111-4111-8111-eeeeeeeeeeee"
)

var goalID = gateway.GoalID(uuid.MustParse(orgID), "ship-it").String()

func seed(t *testing.T) (*gatewatestFake, *Handler, *fakeWS) {
	t.Helper()
	f := gatewaytest.New("rs_test")
	f.Accounts[acctID] = map[string]any{"id": acctID, "login": "acme"}
	f.Projects[projID] = map[string]any{"id": projID, "orgId": orgID, "slug": "mesh", "name": "Mesh"}
	f.Repos[repoID] = map[string]any{"id": repoID, "accountId": acctID, "name": "widgets", "defaultBranch": "main"}
	f.Goals[goalID] = map[string]any{"id": goalID, "orgId": orgID, "projectId": projID, "slug": "ship-it", "title": "Ship it",
		"statement": "Make the thing.", "acceptance": "go test ./... passes", "budgetCents": 1000, "spentCents": 0, "status": "running"}
	f.WorkItems[item1] = map[string]any{"id": item1, "goalId": goalID, "orgId": orgID, "seq": 1, "title": "Write the failing test",
		"brief": "A test for the thing.", "acceptance": "the test fails", "repoId": repoID, "baseRef": "main", "status": "ready"}
	f.Documents["acme/mesh"] = []map[string]any{
		{"slug": "prd", "title": "PRD", "kind": "prd", "repo": "acme/widgets", "path": "specs/prd.md"},
		{"slug": "agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/widgets", "path": "AGENTS.md"},
	}
	f.Bodies["acme/mesh/prd"] = "# PRD"
	f.Bodies["acme/mesh/agents"] = "# AGENTS"
	t.Cleanup(f.Close)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &node.Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: 1}
	if err := gw.ClaimWorkItem(context.Background(), item1, cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{}
	h := &Handler{GW: gw, Cfg: cfg, Scopes: node.Scopes{{Login: "acme", ID: orgID, AccountID: acctID, NodeID: cfg.NodeID}}, WS: ws, WorkRoot: t.TempDir()}
	return &gatewatestFake{f}, h, ws
}

type gatewatestFake struct{ *gatewaytest.Fake }

type fakeWS struct {
	mu      sync.Mutex
	commits []string
}

func (w *fakeWS) Prepare(context.Context, node.WorkspaceSpec) (string, error) { return "", nil }
func (w *fakeWS) Push(context.Context, string, string) (string, error)        { return "", nil }
func (w *fakeWS) Diff(context.Context, string, string) (string, error)        { return "", nil }
func (w *fakeWS) Fetch(context.Context, string, string) error                 { return nil }
func (w *fakeWS) Commit(_ context.Context, dir string, paths []string, msg string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commits = append(w.commits, msg)
	return nil
}

func call(t *testing.T, h *Handler, name string, args map[string]any) string {
	t.Helper()
	for _, tool := range h.Tools() {
		if tool.Name == name {
			out, err := tool.Call(context.Background(), args)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return out
		}
	}
	t.Fatalf("no tool %s", name)
	return ""
}

func TestToolsAgainstGateway(t *testing.T) {
	f, h, ws := seed(t)
	var done []string
	h.OnDone = func(id, outcome string) { done = append(done, id+":"+outcome) }
	h.SetSessionID("s-9")

	call(t, h, "mesh_progress", map[string]any{"item_id": item1, "note": "started", "cost_cents": float64(30)})
	if row := f.Item(item1); row["status"] != "running" || row["costCents"] != 30 || row["lastNote"] != "started" || row["sessionId"] != "s-9" {
		t.Fatalf("after progress: %v", row)
	}
	if out := call(t, h, "reply", map[string]any{"text": "on it", "item_id": item1}); out != "noted on the item" || f.Item(item1)["lastNote"] != "on it" {
		t.Fatalf("reply: %q %v", out, f.Item(item1))
	}
	cx := call(t, h, "mesh_context", map[string]any{"goal_slug": "ship-it"})
	if !strings.Contains(cx, "<!-- instructions-acme-widgets-agents.md -->\n# AGENTS") || !strings.Contains(cx, "# PRD") {
		t.Fatalf("context:\n%s", cx)
	}
	items := call(t, h, "mesh_items", map[string]any{"status": "running"})
	if !strings.Contains(items, item1+" · seq 1 · running · Write the failing test") {
		t.Fatalf("items:\n%s", items)
	}
	if out := call(t, h, "mesh_items", map[string]any{"status": "merged"}); out != "no items" {
		t.Fatalf("items merged: %q", out)
	}
	rec := call(t, h, "mesh_record", map[string]any{"item_id": item1, "summary": "test written"})
	if rec != "recorded specs/goal-ship-it.md on the branch" {
		t.Fatalf("record: %q", rec)
	}
	doc, err := os.ReadFile(filepath.Join(h.WorkRoot, "ship-it", "1", "specs", "goal-ship-it.md"))
	if err != nil || !strings.Contains(string(doc), "parent: prd\n") || !strings.Contains(string(doc), "· item 1 · test written") {
		t.Fatalf("goal doc: %v\n%s", err, doc)
	}
	if len(ws.commits) != 1 || ws.commits[0] != "docs(mesh): goal ship-it — item 1" {
		t.Fatalf("commits: %v", ws.commits)
	}
	call(t, h, "mesh_finish", map[string]any{"item_id": item1, "branch": "mesh/ship-it-1", "head_sha": "abc123"})
	if row := f.Item(item1); row["status"] != "evaluating" || row["headSha"] != "abc123" {
		t.Fatalf("after finish: %v", row)
	}
	call(t, h, "mesh_evaluate", map[string]any{"item_id": item1, "passed": true, "notes": "fails as it should"})
	if f.Item(item1)["status"] != "review" {
		t.Fatalf("after evaluate: %v", f.Item(item1))
	}
	// a fail on another node's item is the gateway's refusal, surfaced
	f.Refuse["failWorkItem"] = "item is review, not running"
	for _, tool := range h.Tools() {
		if tool.Name == "mesh_fail" {
			if _, err := tool.Call(context.Background(), map[string]any{"item_id": item1, "error": "x"}); err == nil || !strings.Contains(err.Error(), "not running") {
				t.Fatalf("mesh_fail must surface the refusal, got %v", err)
			}
		}
	}
	if strings.Join(done, ",") != item1+":finished" {
		t.Fatalf("done = %v", done)
	}
}

// A fake session: speaks MCP to the server over pipes, receives a pushed
// event, calls a tool.
func TestServerWithFakeSession(t *testing.T) {
	f, h, _ := seed(t)
	srv := &Server{Instructions: Instructions, Tools: h.Tools()}
	toServerR, toServerW := io.Pipe()     // session → server
	fromServerR, fromServerW := io.Pipe() // server → session
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, toServerR, fromServerW) }()
	reader := bufio.NewReader(fromServerR)
	send := func(v map[string]any) {
		b, _ := json.Marshal(v)
		if _, err := toServerW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() map[string]any {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		return m
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}})
	init := recv()
	res := init["result"].(map[string]any)
	caps := res["capabilities"].(map[string]any)
	if _, ok := caps["experimental"].(map[string]any)["claude/channel"]; !ok {
		t.Fatalf("initialize must declare claude/channel: %v", init)
	}
	if res["instructions"] != Instructions || res["serverInfo"].(map[string]any)["name"] != "mesh" {
		t.Fatalf("initialize: %v", init)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	list := recv()
	names := []string{}
	for _, tl := range list["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "mesh_progress,mesh_finish,mesh_fail,mesh_evaluate,mesh_context,mesh_items,mesh_record,reply" {
		t.Fatalf("tools: %v", names)
	}

	// the daemon pushes an event; the session sees the notification
	resolved := &node.Resolved{Item: gateway.WorkItem{ID: item1, Seq: 1, Title: "Write the failing test", Brief: "A test.", Acceptance: "fails"},
		Goal: &gateway.Goal{Slug: "ship-it", Title: "Ship it"}, Repo: &gateway.Repo{Name: "widgets", Owner: "acme", DefaultBranch: "main"}}
	content, meta := ReadyEvent(resolved, "/tmp/ws")
	pushErr := make(chan error, 1)
	go func() { pushErr <- srv.Push(content, meta) }() // the pipe write completes when the session reads
	ev := recv()
	if err := <-pushErr; err != nil {
		t.Fatal(err)
	}
	if ev["method"] != "notifications/claude/channel" {
		t.Fatalf("event: %v", ev)
	}
	params := ev["params"].(map[string]any)
	m := params["meta"].(map[string]any)
	if m["kind"] != "work_item.ready" || m["item_id"] != item1 || m["seq"] != "1" || m["goal_slug"] != "ship-it" || m["repo"] != "acme/widgets" || m["base_ref"] != "main" || m["branch"] != "mesh/ship-it-1" || m["workspace"] != "/tmp/ws" {
		t.Fatalf("meta: %v", m)
	}
	if !strings.Contains(params["content"].(string), "A test.") || !strings.Contains(params["content"].(string), "mesh_context(goal_slug=\"ship-it\")") {
		t.Fatalf("content: %v", params["content"])
	}

	// the session calls back
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "mesh_progress", "arguments": map[string]any{"item_id": item1, "note": "cloning", "cost_cents": 5}}})
	out := recv()
	if out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "recorded" {
		t.Fatalf("tools/call: %v", out)
	}
	if row := f.Item(item1); row["status"] != "running" || row["lastNote"] != "cloning" || row["costCents"] != 5 {
		t.Fatalf("gateway after the session's call: %v", row)
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "mesh_fail", "arguments": map[string]any{"item_id": item1}}})
	if e := recv(); e["result"].(map[string]any)["isError"] != true {
		t.Fatalf("a bad call must be a tool error: %v", e)
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "nope"})
	if e := recv(); e["error"] == nil {
		t.Fatalf("unknown method must error: %v", e)
	}
	toServerW.Close()
}

func TestHubQueuesUntilConnected(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "node.sock")
	var mu sync.Mutex
	var done []string
	hub := &Hub{OnDone: func(id, outcome string) { mu.Lock(); done = append(done, id+":"+outcome); mu.Unlock() }}
	if err := hub.Listen(sock); err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	hub.Push("early", map[string]string{"kind": "checkpoint"})

	got := make(chan string, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, sock, func(content string, meta map[string]string) { got <- content + "/" + meta["kind"] })
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if v := <-got; v != "early/checkpoint" {
		t.Fatalf("queued event = %q", v)
	}
	waitFor(t, hub.Connected)
	hub.Push("live", map[string]string{"kind": "goal.paused"})
	if v := <-got; v != "live/goal.paused" {
		t.Fatalf("live event = %q", v)
	}
	if err := conn.Done(item1, "finished"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(done) == 1 && done[0] == item1+":finished" })
}

func TestGoalEventsAndSystemPrompt(t *testing.T) {
	content, meta := GoalEvent("goal.paused", gateway.Goal{Slug: "ship-it", Title: "Ship it", Status: "paused", PausedReason: "over budget"})
	if !strings.Contains(content, "over budget") || meta["kind"] != "goal.paused" || meta["goal_slug"] != "ship-it" {
		t.Fatalf("%q %v", content, meta)
	}
	s := &Session{Cfg: &node.Config{Slug: "n", OrgLogin: "acme"}, Unattended: true}
	args := strings.Join(s.Args("/w"), " ")
	for _, want := range []string{"-p --input-format stream-json", "--output-format stream-json", "--dangerously-load-development-channels server:mesh", "--append-system-prompt", "mesh_record(item_id, summary)", "--dangerously-skip-permissions"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q", want)
		}
	}
	if strings.Contains(strings.Join((&Session{Cfg: s.Cfg}).Args("/w"), " "), "--dangerously-skip-permissions") {
		t.Error("attended session must not skip permissions")
	}
}

func itemFixture() gateway.WorkItem {
	return gateway.WorkItem{ID: item1, Seq: 1, Title: "Write the failing test", Brief: "A test.", Acceptance: "fails"}
}
func goalFixture() *gateway.Goal { return &gateway.Goal{Slug: "ship-it", Title: "Ship it"} }
func repoFixture() *gateway.Repo {
	return &gateway.Repo{Name: "widgets", Owner: "acme", DefaultBranch: "main"}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
