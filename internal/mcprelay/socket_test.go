package mcprelay

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// proxyGateway is a fake sessionproxy, served on a unix socket at
// /mcp/<name>: no bearer is asked of a caller — the proxy, not the
// session reaching it, holds whatever credential the real upstream
// needs.
type proxyGateway struct {
	mu       sync.Mutex
	name     string
	sessions map[string]bool
	next     int
	calls    []string // method, path and Authorization header seen
}

func (g *proxyGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
	if r.URL.Path != SocketPrefix+g.name {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if m.Method == "initialize" {
		g.next++
		sid := fmt.Sprintf("s%d", g.next)
		g.sessions[sid] = true
		w.Header().Set("Mcp-Session-Id", sid)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"fake"}}}`, m.ID)
		return
	}
	if !g.sessions[r.Header.Get("Mcp-Session-Id")] {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if len(m.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok on the socket"}]}}`, m.ID)
}

// TestProxySocketRelay is `mesh mcp-relay --socket S --server N`: a
// Relay in Socket mode reaches one upstream of a sessionproxy over a
// unix socket, SocketPrefix+Server, with no TokenFile and so no
// Authorization header on any request.
func TestProxySocketRelay(t *testing.T) {
	gw := &proxyGateway{name: "search", sessions: map[string]bool{}}
	socket := filepath.Join(t.TempDir(), "proxy.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: gw}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	c := startRelay(t, &Relay{Socket: socket, Server: "search", Stderr: io.Discard})
	c.handshake()
	c.send(fmt.Sprintf(progressCall, 2))
	if got := toolText(t, c.reply(2)); got != "ok on the socket" {
		t.Fatalf("tools/call over the socket: %q", got)
	}

	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.calls) == 0 {
		t.Fatal("the gateway saw no request")
	}
	for _, call := range gw.calls {
		if !strings.HasPrefix(call, "POST "+SocketPrefix+"search auth=") {
			t.Fatalf("unexpected request: %q", call)
		}
		if !strings.HasSuffix(call, "auth=") {
			t.Fatalf("a request carried a credential: %q", call)
		}
	}
}
