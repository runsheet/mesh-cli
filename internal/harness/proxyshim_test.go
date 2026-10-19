package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// proxyShimGateway is a fake sessionproxy, served on a unix socket at
// /mcp/<name>: it hands out Mcp-Session-Ids on initialize and answers
// 404 for a session it does not know. No bearer, no header at all, is
// asked of a caller — the proxy, not the shim, holds any credential.
type proxyShimGateway struct {
	mu       sync.Mutex
	name     string
	sessions map[string]bool
	next     int
	inits    int
}

func (g *proxyShimGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Path != "/mcp/"+g.name {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
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
	writeRelayReply(w, g.name == "sse", map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "progress noted in " + r.Header.Get("Mcp-Session-Id")}}}})
}

// startProxyShim runs ProxyShimScript under node against the proxy's
// socket, as ProxiedServer would have a pod run it.
func startProxyShim(t *testing.T, socket, name string) *relayProc {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; ProxyShimScript runs under node, so this test needs it")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, ProxyShimScriptName)
	if err := os.WriteFile(script, ProxyShimScript(), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nodeBin, script, socket, name)
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

func listenUnix(t *testing.T, gw http.Handler) string {
	t.Helper()
	// A short dir of its own, not t.TempDir(): nested under the test's
	// own name, that can run past AF_UNIX's ~108-byte sun_path, as
	// internal/sessionproxy's own proxy_test.go avoids the same way.
	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "proxy.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: gw}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return socket
}

func TestProxyShimRelaysInitializeAndToolsCall(t *testing.T) {
	gw := &proxyShimGateway{name: "search", sessions: map[string]bool{}}
	socket := listenUnix(t, gw)

	p := startProxyShim(t, socket, "search")
	p.initialize()
	p.send(callProgress(2))
	r := p.reply()
	if r["id"] != float64(2) || !strings.HasPrefix(resultText(t, r), "progress noted") {
		t.Fatalf("tools/call: %v", r)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.inits != 1 {
		t.Fatalf("initialize sent %d times, want 1", gw.inits)
	}
}

func TestProxyShimRelaysAnEventStreamReply(t *testing.T) {
	gw := &proxyShimGateway{name: "sse", sessions: map[string]bool{}}
	socket := listenUnix(t, gw)

	p := startProxyShim(t, socket, "sse")
	p.initialize()
	p.send(callProgress(2))
	r := p.reply()
	if r["id"] != float64(2) || !strings.HasPrefix(resultText(t, r), "progress noted") {
		t.Fatalf("tools/call over an event stream: %v", r)
	}
}

// The shim reads no token file and carries no secret: it is started on
// a socket path and a server name alone, and its own source names
// neither a token nor a bearer.
func TestProxyShimScriptNamesNoTokenFile(t *testing.T) {
	src := strings.ToLower(string(ProxyShimScript()))
	for _, bad := range []string{"token", "bearer", "authorization"} {
		if strings.Contains(src, bad) {
			t.Fatalf("ProxyShimScript names %q:\n%s", bad, src)
		}
	}
}

func TestProxiedServerNamesOnlySocketAndServer(t *testing.T) {
	got := ProxiedServer("/tmp/mesh-home-x/mcp-proxy-shim.js", "/tmp/mesh-home-x/proxy.sock", "search")
	args, _ := got["args"].([]string)
	if got["command"] != "node" || len(args) != 3 ||
		args[0] != "/tmp/mesh-home-x/mcp-proxy-shim.js" || args[1] != "/tmp/mesh-home-x/proxy.sock" || args[2] != "search" {
		t.Fatalf("ProxiedServer = %v", got)
	}
	js, _ := json.Marshal(got)
	for _, bad := range []string{"token", "Bearer", "secret"} {
		if strings.Contains(string(js), bad) {
			t.Fatalf("ProxiedServer entry names %q: %s", bad, js)
		}
	}
}
