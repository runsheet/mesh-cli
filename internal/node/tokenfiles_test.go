package node

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/mcprelay"
)

// fakeBearer is a rotating machine credential on a fake clock: the
// bearer it hands out until rotateAt, the next after, as auth.Source
// refreshes near expiry.
type fakeBearer struct {
	mu       sync.Mutex
	now      time.Time
	rotateAt time.Time
	old, new string
}

func (b *fakeBearer) Token(context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.now.Before(b.rotateAt) {
		return b.old, nil
	}
	return b.new, nil
}

func (b *fakeBearer) advance(d time.Duration) {
	b.mu.Lock()
	b.now = b.now.Add(d)
	b.mu.Unlock()
}

func readToken(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// Keep writes the bearer at once, 0600; the file is rewritten as the
// bearer rotates — before the old one's life is out, by its own ticker —
// and removed when the session's end is handled. A release that leaves
// the file (a detach) stops the refresh and keeps it.
func TestTokenFileRewrittenBeforeExpiryAndRemovedAtTheEnd(t *testing.T) {
	start := time.Date(2026, 10, 11, 4, 30, 0, 0, time.UTC)
	b := &fakeBearer{now: start, rotateAt: start.Add(time.Hour), old: "rs_old", new: "rs_new"}
	tokens := &TokenFiles{Token: b.Token, Every: 10 * time.Millisecond}
	path := filepath.Join(t.TempDir(), "goal", "1.session", harness.TokenFileName)
	release, err := tokens.Keep(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := readToken(t, path); got != "rs_old" {
		t.Fatalf("token file = %q", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", fi.Mode().Perm())
	}
	// the credential refreshes ahead of its expiry: the file follows on
	// the next tick, while the old bearer would still have been good
	b.advance(time.Hour)
	deadline := time.Now().Add(5 * time.Second)
	for readToken(t, path) != "rs_new" {
		if time.Now().After(deadline) {
			t.Fatal("token file not rewritten after the rotation")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Fatalf("temporary files left beside the token file: %v", ents)
	}
	if got := tokens.Kept(); len(got) != 1 || got[0] != path {
		t.Fatalf("kept %v", got)
	}
	release(true)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("token file left after the session's end: %v", err)
	}
	if got := tokens.Kept(); len(got) != 0 {
		t.Fatalf("still kept after release: %v", got)
	}

	// a detach: released without removal, refreshed no more
	path2 := filepath.Join(t.TempDir(), harness.TokenFileName)
	release2, err := tokens.Keep(path2)
	if err != nil {
		t.Fatal(err)
	}
	release2(false)
	os.WriteFile(path2, []byte("rs_left\n"), 0o600)
	time.Sleep(50 * time.Millisecond)
	if got := readToken(t, path2); got != "rs_left" {
		t.Fatalf("a released file was still refreshed: %q", got)
	}

	// no bearer: no file, an error
	none := &TokenFiles{Token: func(context.Context) (string, error) { return "", nil }}
	if _, err := none.Keep(filepath.Join(t.TempDir(), harness.TokenFileName)); err == nil {
		t.Fatal("an empty bearer was kept")
	}
}

// rpcClient speaks newline-delimited JSON-RPC to a relay on pipes.
type rpcClient struct {
	t   *testing.T
	in  io.Writer
	out *bufio.Scanner
}

func (c *rpcClient) send(m map[string]any) {
	c.t.Helper()
	m["jsonrpc"] = "2.0"
	b, _ := json.Marshal(m)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

// call sends a request and reads lines until its reply.
func (c *rpcClient) call(id int, method string, params map[string]any) map[string]any {
	c.t.Helper()
	c.send(map[string]any{"id": id, "method": method, "params": params})
	for c.out.Scan() {
		var m map[string]any
		if json.Unmarshal(c.out.Bytes(), &m) != nil {
			continue
		}
		if got, _ := m["id"].(float64); int(got) == id {
			return m
		}
	}
	c.t.Fatalf("relay closed before replying to %d %s: %v", id, method, c.out.Err())
	return nil
}

// toolText is a tools/call reply's text, failing on an error reply.
func (c *rpcClient) tool(id int, name string, args map[string]any) string {
	c.t.Helper()
	m := c.call(id, "tools/call", map[string]any{"name": name, "arguments": args})
	if m["error"] != nil {
		c.t.Fatalf("%s: %v", name, m["error"])
	}
	res, _ := m["result"].(map[string]any)
	content, _ := res["content"].([]any)
	text := ""
	if len(content) > 0 {
		text, _ = content[0].(map[string]any)["text"].(string)
	}
	if isErr, _ := res["isError"].(bool); isErr {
		c.t.Fatalf("%s answered an error: %s", name, text)
	}
	return text
}

// The goal's proof (mesh-cli ADR-029): a session started 50 minutes
// before the node's token rotates — its mesh-context the relay
// MeshContextMCP renders, placed on a token file in its session dir as
// the harness places it and kept by TokenFiles — still calls
// mesh_progress and mesh_finish successfully after the rotation, when
// the fake /mcp refuses the old token.
func TestSessionStarted50MinutesBeforeRotationFinishesAfterIt(t *testing.T) {
	f := gatewaytest.New("rs_old")
	f.HostedWorkTools = true
	const itemID = "item-3"
	f.WorkItems[itemID] = map[string]any{"id": itemID, "goalId": "goal-1", "seq": 3, "title": "Outlive the token",
		"status": "running", "nodeId": "node-1", "branch": "mesh/outlive-3", "dependsOn": []string{}}

	start := time.Date(2026, 10, 11, 4, 30, 0, 0, time.UTC)
	b := &fakeBearer{now: start, rotateAt: start.Add(50 * time.Minute), old: "rs_old", new: "rs_new"}
	// Every an hour: the test drives each refresh on its fake clock
	tokens := &TokenFiles{Token: b.Token, Every: time.Hour}

	sessionDir := filepath.Join(t.TempDir(), "goal", "3.session")
	servers, path, release, err := harness.PlaceMeshContext(MeshContextMCP("mesh", f.URL()), tokens, sessionDir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := servers[harness.BuiltinMCP].(map[string]any)
	args := srv["args"].([]string)
	if srv["type"] != "stdio" || len(args) != 5 || args[0] != harness.RelayCommand || args[2] != f.URL()+"/mcp" || args[4] != path {
		t.Fatalf("mesh-context = %v", srv)
	}
	for _, a := range args {
		if strings.Contains(a, "rs_old") {
			t.Fatalf("the bearer rides the session's config: %v", args)
		}
	}

	// the session's relay, as `mesh mcp-relay --url … --token-file …` runs
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&mcprelay.Relay{URL: args[2], TokenFile: args[4], Stderr: io.Discard}).Run(ctx, inR, outW)
		outW.Close()
	}()
	c := &rpcClient{t: t, in: inW, out: bufio.NewScanner(outR)}
	if m := c.call(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code"}}); m["result"] == nil {
		t.Fatalf("initialize: %v", m)
	}
	c.send(map[string]any{"method": "notifications/initialized"})
	if got := c.tool(2, "mesh_progress", map[string]any{"item": itemID, "note": "session started"}); got != "noted" {
		t.Fatalf("mesh_progress before the rotation = %q", got)
	}

	// 55 minutes on: the credential has rotated and the platform
	// refuses the old bearer
	b.advance(55 * time.Minute)
	f.SetToken("rs_new")
	req, _ := http.NewRequest(http.MethodPost, f.URL()+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer rs_old")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old bearer after the rotation: %d, want 401", resp.StatusCode)
	}
	if err := tokens.Refresh(); err != nil {
		t.Fatal(err)
	}
	if got := readToken(t, path); got != "rs_new" {
		t.Fatalf("token file after the refresh = %q", got)
	}

	if got := c.tool(3, "mesh_progress", map[string]any{"item": itemID, "note": "still here after the rotation"}); got != "noted" {
		t.Fatalf("mesh_progress after the rotation = %q", got)
	}
	if got := c.tool(4, "mesh_finish", map[string]any{"item": itemID, "branch": "mesh/outlive-3", "headSha": "0123456789abcdef0123456789abcdef01234567"}); got != "finished: the item is evaluating" {
		t.Fatalf("mesh_finish after the rotation = %q", got)
	}
	finishes := f.CallsNamed("mesh_finish")
	if len(finishes) != 1 || finishes[0].Bearer != "rs_new" {
		t.Fatalf("mesh_finish calls %+v, want one on the rotated bearer", finishes)
	}
	if progress := f.CallsNamed("mesh_progress"); len(progress) != 2 || progress[0].Bearer != "rs_old" || progress[1].Bearer != "rs_new" {
		t.Fatalf("mesh_progress calls %+v", progress)
	}
	if got := f.Item(itemID)["status"]; got != "evaluating" {
		t.Fatalf("item is %v after the finish", got)
	}

	inW.Close()
	cancel()
	<-done
	release(true)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("token file left after the session's end: %v", err)
	}
}
