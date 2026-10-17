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

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/mcprelay"
)

// tokenClock is the node's clock in a test: every kept file's ticker
// (TokenFiles.Ticker) and the bearer read the same now, and step moves
// it one tick on and waits until every kept file has handled it.
type tokenClock struct {
	t     *testing.T
	mu    sync.Mutex
	now   time.Time
	every time.Duration
	ticks []chan time.Time
}

func (c *tokenClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *tokenClock) Ticker(every time.Duration) (<-chan time.Time, func()) {
	ch := make(chan time.Time)
	c.mu.Lock()
	c.every = every
	c.ticks = append(c.ticks, ch)
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i, x := range c.ticks {
			if x == ch {
				c.ticks = append(c.ticks[:i], c.ticks[i+1:]...)
				return
			}
		}
	}
}

// waitForTicker blocks until Keep's refresh goroutine has registered
// its ticker (Ticker runs in a goroutine Keep starts and returns
// before): without it, a step called right after Keep can race the
// registration, find no ticks to deliver to, and return having moved
// the clock but told nobody — the token never refreshed and the
// assertion right after it flaky.
func (c *tokenClock) waitForTicker() {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.ticks)
		c.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatal("a kept file's refresher never registered its ticker")
		}
		time.Sleep(time.Millisecond)
	}
}

// step moves the clock one Every on and delivers the tick to every kept
// file's refresher twice: the second is taken only once the first's
// refresh is done, so the files are what the tick made of them.
func (c *tokenClock) step() time.Time {
	c.t.Helper()
	c.mu.Lock()
	c.now = c.now.Add(c.every)
	now := c.now
	ticks := append([]chan time.Time(nil), c.ticks...)
	c.mu.Unlock()
	for _, ch := range ticks {
		for range 2 {
			select {
			case ch <- now:
			case <-time.After(5 * time.Second):
				c.t.Fatal("a kept file's refresher took no tick")
			}
		}
	}
	return now
}

// fakeBearer is gw.Bearer over a rotating machine credential on the
// fake clock, as auth.Source hands it out: the old access token until
// it is within auth.RefreshSkew of expiry (auth.NeedsRefresh), then the
// renewed one.
type fakeBearer struct {
	clock    *tokenClock
	expiry   time.Time
	old, new string
}

func (b *fakeBearer) Token(context.Context) (string, error) {
	if b.clock.Now().Add(auth.RefreshSkew).After(b.expiry) {
		return b.new, nil
	}
	return b.old, nil
}

func readToken(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// fileAtExpiry keeps a token file from start on a fake clock ticking
// every every, the bearer expiring at expiry, and is what the file
// held at the last tick before the expiry — what a relay reading it
// then sends — with the kept path and its release.
func fileAtExpiry(t *testing.T, start, expiry time.Time, every time.Duration) (string, string, func(bool)) {
	t.Helper()
	clock := &tokenClock{t: t, now: start, every: every}
	b := &fakeBearer{clock: clock, expiry: expiry, old: "rs_old", new: "rs_new"}
	tokens := &TokenFiles{Token: b.Token, Every: every, Ticker: clock.Ticker}
	path := filepath.Join(t.TempDir(), "goal", "1.session", harness.TokenFileName)
	release, err := tokens.Keep(path)
	if err != nil {
		t.Fatal(err)
	}
	clock.waitForTicker()
	held := readToken(t, path)
	for {
		if now := clock.step(); !now.Before(expiry) {
			return held, path, release
		}
		held = readToken(t, path)
	}
}

// The token file is rewritten before the old bearer's expiry by its own
// ticker, whatever the phase of the ticks against the credential's
// renewal — the boundary included, where a tick lands exactly
// auth.RefreshSkew before expiry and is still handed the old bearer —
// and removed when the session's end is handled. A release that leaves
// the file (a detach) stops the refresh and keeps it; a refresh never
// makes a removed session directory again.
func TestTokenFileRewrittenBeforeExpiryAndRemovedAtTheEnd(t *testing.T) {
	if TokenRefreshEvery >= auth.RefreshSkew {
		t.Fatalf("TokenRefreshEvery %v is not inside the credential's refresh skew %v", TokenRefreshEvery, auth.RefreshSkew)
	}
	start := time.Date(2026, 10, 11, 4, 30, 0, 0, time.UTC)
	expiry := start.Add(time.Hour)
	// every phase of the ticks against the expiry, a second apart: phase
	// 0 puts a tick on expiry − RefreshSkew exactly
	for phase := time.Duration(0); phase < TokenRefreshEvery; phase += time.Second {
		held, path, release := fileAtExpiry(t, start.Add(phase), expiry, TokenRefreshEvery)
		if held != "rs_new" {
			t.Fatalf("phase %v: the token file held %q at the last tick before expiry, want the renewed bearer", phase, held)
		}
		release(true)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("phase %v: token file left after the session's end: %v", phase, err)
		}
	}
	// the interval the first attempt had, the skew itself, misses it:
	// the tick at expiry − skew gets the old bearer and the next is at
	// the expiry
	if held, _, release := fileAtExpiry(t, start, expiry, auth.RefreshSkew); held != "rs_old" {
		t.Fatalf("a refresh every RefreshSkew held %q before expiry; the boundary is not exercised", held)
	} else {
		release(true)
	}

	clock := &tokenClock{t: t, now: start, every: TokenRefreshEvery}
	b := &fakeBearer{clock: clock, expiry: start.Add(auth.RefreshSkew + TokenRefreshEvery/2), old: "rs_old", new: "rs_new"}
	tokens := &TokenFiles{Token: b.Token, Ticker: clock.Ticker}
	dir := filepath.Join(t.TempDir(), "goal", "1.session")
	path := filepath.Join(dir, harness.TokenFileName)
	release, err := tokens.Keep(path)
	if err != nil {
		t.Fatal(err)
	}
	clock.waitForTicker()
	if got := readToken(t, path); got != "rs_old" {
		t.Fatalf("token file = %q", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", fi.Mode().Perm())
	}
	clock.step()
	if got := readToken(t, path); got != "rs_new" {
		t.Fatalf("token file after the renewal = %q", got)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("temporary files left beside the token file: %v", ents)
	}
	if got := tokens.Kept(); len(got) != 1 || got[0] != path {
		t.Fatalf("kept %v", got)
	}
	// the session directory goes under a kept file: a later rotation
	// does not make it again
	os.RemoveAll(dir)
	b.new = "rs_newer"
	clock.step()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a refresh made the removed session directory again: %v", err)
	}
	release(true)
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
	clock.step()
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
// before the node's token rotates — its runsheet the relay
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
	expiry := start.Add(50 * time.Minute)
	clock := &tokenClock{t: t, now: start, every: TokenRefreshEvery}
	b := &fakeBearer{clock: clock, expiry: expiry, old: "rs_old", new: "rs_new"}
	// the node's own refresh interval on the fake clock: nothing in this
	// test rewrites the file but the keeper's own ticks
	tokens := &TokenFiles{Token: b.Token, Ticker: clock.Ticker}

	sessionDir := filepath.Join(t.TempDir(), "goal", "3.session")
	servers, path, release, err := harness.PlaceMeshContext(MeshContextMCP("mesh", f.URL()), tokens, sessionDir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := servers[harness.BuiltinMCP].(map[string]any)
	args := srv["args"].([]string)
	if srv["type"] != "stdio" || len(args) != 5 || args[0] != harness.RelayCommand || args[2] != f.URL()+"/mcp" || args[4] != path {
		t.Fatalf("runsheet = %v", srv)
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

	// 55 minutes on, a tick at a time: the credential renews a minute
	// before its expiry and, from the expiry, the platform refuses the
	// old bearer
	for clock.Now().Before(start.Add(55 * time.Minute)) {
		if !clock.step().Before(expiry) {
			f.SetToken("rs_new")
		}
	}
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
	if got := readToken(t, path); got != "rs_new" {
		t.Fatalf("token file after the rotation = %q", got)
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
