package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/mcprelay"
)

// relayGateway is a fake /mcp: it accepts one bearer at a time, hands out
// Mcp-Session-Ids on initialize and answers 404 for a session it does not
// know. onRefused, when set, runs as a bearer is refused (a rotation that
// lands mid-request).
type relayGateway struct {
	mu        sync.Mutex
	accept    string
	sessions  map[string]bool
	next      int
	refused   int
	inits     int
	onRefused func()
	sse       bool
}

func (g *relayGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+g.accept {
		g.refused++
		if g.onRefused != nil {
			g.onRefused()
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var m map[string]any
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &m); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	method, _ := m["method"].(string)
	if method == "initialize" {
		g.inits++
		g.next++
		sid := fmt.Sprintf("s%d", g.next)
		g.sessions[sid] = true
		w.Header().Set("Mcp-Session-Id", sid)
		writeRelayReply(w, false, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake"}}})
		return
	}
	if !g.sessions[r.Header.Get("Mcp-Session-Id")] {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if _, ok := m["id"]; !ok || method == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRelayReply(w, g.sse, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "progress noted in " + r.Header.Get("Mcp-Session-Id")}}}})
}

func writeRelayReply(w http.ResponseWriter, sse bool, m any) {
	b, _ := json.Marshal(m)
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// relayProc is RelayScript running under node against the node's relay
// endpoint, which forwards to a gateway on the bearer in a token file.
type relayProc struct {
	t     *testing.T
	in    io.WriteCloser
	lines chan string
}

func startRelay(t *testing.T, url, tokenFile string) *relayProc {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; RelayScript runs under node, so this test needs it")
	}
	// a socket path past sun_path's 108 bytes: the endpoint binds it by
	// its directory and the script reaches it from there
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, relayScriptName)
	if err := os.WriteFile(script, RelayScript(), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, RelaySocketName)
	ep, err := mcprelay.Serve(sock, url, tokenFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ep.Close() })
	cmd := exec.Command(nodeBin, script, sock)
	in, _ := cmd.StdinPipe()
	outPipe, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); _ = cmd.Wait() })
	p := &relayProc{t: t, in: in, lines: make(chan string, 16)}
	go func() {
		sc := bufio.NewScanner(outPipe)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	return p
}

func (p *relayProc) send(m map[string]any) {
	p.t.Helper()
	m["jsonrpc"] = "2.0"
	b, _ := json.Marshal(m)
	if _, err := p.in.Write(append(b, '\n')); err != nil {
		p.t.Fatal(err)
	}
}

func (p *relayProc) reply() map[string]any {
	p.t.Helper()
	select {
	case l, ok := <-p.lines:
		if !ok {
			p.t.Fatal("relay exited")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			p.t.Fatalf("relay wrote %q: %v", l, err)
		}
		return m
	case <-time.After(10 * time.Second):
		p.t.Fatal("no reply from the relay in 10s")
	}
	return nil
}

func (p *relayProc) initialize() {
	p.t.Helper()
	p.send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}})
	if r := p.reply(); r["result"] == nil {
		p.t.Fatalf("initialize: %v", r)
	}
	p.send(map[string]any{"method": "notifications/initialized"})
}

func callProgress(id int) map[string]any {
	return map[string]any{"id": id, "method": "tools/call", "params": map[string]any{"name": "mesh_progress", "arguments": map[string]any{"note": "still here"}}}
}

func resultText(t *testing.T, r map[string]any) string {
	t.Helper()
	res, ok := r["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", r)
	}
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content: %v", r)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

func writeToken(t *testing.T, path, tok string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRelayScriptRotatedToken(t *testing.T) {
	g := &relayGateway{accept: "t1", sessions: map[string]bool{}}
	srv := httptest.NewServer(g)
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "t1")
	p := startRelay(t, srv.URL, tokenFile)
	p.initialize()

	// The node rotates: the file is rewritten and the old token refused.
	writeToken(t, tokenFile, "t2")
	g.mu.Lock()
	g.accept = "t2"
	g.sse = true
	g.mu.Unlock()
	p.send(callProgress(2))
	if r := p.reply(); r["id"] != float64(2) || !strings.HasPrefix(resultText(t, r), "progress noted") {
		t.Fatalf("after rotation: %v", r)
	}

	// A rotation that lands mid-request: the token read is refused, the
	// file rewritten, and the relay's one re-read-and-retry carries it.
	g.mu.Lock()
	g.accept = "t3"
	g.onRefused = func() { writeToken(t, tokenFile, "t3"); g.onRefused = nil }
	refusedBefore := g.refused
	g.mu.Unlock()
	p.send(callProgress(3))
	if r := p.reply(); r["id"] != float64(3) || !strings.HasPrefix(resultText(t, r), "progress noted") {
		t.Fatalf("after a 401: %v", r)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refused != refusedBefore+1 {
		t.Fatalf("refused %d times, want one 401 then the retry", g.refused-refusedBefore)
	}
}

func TestRelayScriptReinitializesForgottenSession(t *testing.T) {
	g := &relayGateway{accept: "t1", sessions: map[string]bool{}}
	srv := httptest.NewServer(g)
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "t1")
	p := startRelay(t, srv.URL, tokenFile)
	p.initialize()

	// The gateway restarts and forgets every MCP session.
	g.mu.Lock()
	g.sessions = map[string]bool{}
	g.mu.Unlock()
	p.send(callProgress(2))
	r := p.reply()
	if r["id"] != float64(2) || resultText(t, r) != "progress noted in s2" {
		t.Fatalf("after the session was forgotten: %v", r)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inits != 2 {
		t.Fatalf("initialize sent %d times, want 2 (the client's and the relay's re-initialize)", g.inits)
	}
}

func TestRelayScriptEmptyTokenFile(t *testing.T) {
	g := &relayGateway{accept: "t1", sessions: map[string]bool{}}
	srv := httptest.NewServer(g)
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "t1")
	p := startRelay(t, srv.URL, tokenFile)
	p.initialize()

	if err := os.WriteFile(tokenFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p.send(callProgress(2))
	r := p.reply()
	e, ok := r["error"].(map[string]any)
	if r["id"] != float64(2) || !ok || !strings.Contains(fmt.Sprint(e["message"]), "is empty") {
		t.Fatalf("empty token file: %v", r)
	}

	// A missing one is the same error.
	os.Remove(tokenFile)
	p.send(callProgress(3))
	if r := p.reply(); r["id"] != float64(3) || r["error"] == nil {
		t.Fatalf("missing token file: %v", r)
	}
}

func TestRelayServerInPod(t *testing.T) {
	got := sandboxRelayServer("/tmp/mesh-home-x/"+relayScriptName, "/tmp/mesh-home-x/"+RelaySocketName)
	args, _ := got["args"].([]string)
	if got["command"] != "node" || len(args) != 2 || args[0] != "/tmp/mesh-home-x/"+relayScriptName || args[1] != "/tmp/mesh-home-x/"+RelaySocketName {
		t.Fatalf("sandboxRelayServer = %v", got)
	}
}
