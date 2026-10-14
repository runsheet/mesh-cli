package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateway is a fake /mcp: it accepts one bearer at a time, hands out
// sessions on initialize, answers tools/call as an event stream and
// everything else as JSON.
type gateway struct {
	t *testing.T

	mu       sync.Mutex
	token    string
	sessions map[string]bool
	next     int
	inits    int
	calls    []string // method and bearer of each request
}

func newGateway(t *testing.T, token string) (*gateway, *httptest.Server) {
	g := &gateway{t: t, token: token, sessions: map[string]bool{}}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *gateway) accept(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.token = token
}

// seen waits until the gateway has had n requests.
func (g *gateway) seen(n int) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		got := len(g.calls)
		g.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	g.t.Fatalf("the gateway saw fewer than %d requests", n)
}

func (g *gateway) forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions = map[string]bool{}
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	bearer := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	g.calls = append(g.calls, m.Method+" "+bearer)
	if bearer != g.token {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	if m.Method == "initialize" {
		g.next++
		g.inits++
		id := fmt.Sprintf("s%d", g.next)
		g.sessions[id] = true
		w.Header().Set("Mcp-Session-Id", id)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"fake"}}}`, m.ID)
		return
	}
	if !g.sessions[req.Header.Get("Mcp-Session-Id")] {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if len(m.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if m.Method == "tools/call" {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\n", m.ID)
		fmt.Fprintf(w, "data: \"result\":{\"content\":[{\"type\":\"text\",\"text\":\"%s ok\"}]}}\n\n", m.Params.Name)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, m.ID)
}

// client drives a relay over pipes, one line at a time.
type client struct {
	t   *testing.T
	in  *io.PipeWriter
	out chan map[string]any
}

func startRelay(t *testing.T, r *Relay) *client {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{t: t, in: inW, out: make(chan map[string]any, 16)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(context.Background(), inR, outW)
		outW.Close()
	}()
	go func() {
		dec := json.NewDecoder(outR)
		for {
			var m map[string]any
			if err := dec.Decode(&m); err != nil {
				close(c.out)
				return
			}
			c.out <- m
		}
	}()
	t.Cleanup(func() {
		inW.Close()
		<-done
	})
	return c
}

func (c *client) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

// reply is the next message carrying id, skipping the server's
// notifications.
func (c *client) reply(id float64) map[string]any {
	c.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.out:
			if !ok {
				c.t.Fatalf("relay ended before answering %v", id)
			}
			if m["id"] == id {
				return m
			}
		case <-timeout:
			c.t.Fatalf("no answer to %v", id)
		}
	}
}

func (c *client) handshake() {
	c.t.Helper()
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code"}}}`)
	if m := c.reply(1); m["result"] == nil {
		c.t.Fatalf("initialize: %v", m)
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

const progressCall = `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"mesh_progress","arguments":{"note":"still going"}}}`

func toolText(t *testing.T, m map[string]any) string {
	t.Helper()
	res, _ := m["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no result content: %v", m)
	}
	first, _ := content[0].(map[string]any)
	s, _ := first["text"].(string)
	return s
}

func writeToken(t *testing.T, path, tok string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRelayReadsTheTokenFileOnEveryRequest(t *testing.T) {
	g, srv := newGateway(t, "token-A")
	file := filepath.Join(t.TempDir(), "token")
	writeToken(t, file, "token-A\n")
	c := startRelay(t, &Relay{URL: srv.URL, TokenFile: file, Stderr: io.Discard})

	c.handshake()
	c.send(fmt.Sprintf(progressCall, 2))
	if got := toolText(t, c.reply(2)); got != "mesh_progress ok" {
		t.Fatalf("before the rotation: %q", got)
	}

	// The node rotates: the file says B and the gateway refuses A.
	g.seen(3)
	writeToken(t, file, "  token-B\n")
	g.accept("token-B")

	c.send(fmt.Sprintf(progressCall, 3))
	if got := toolText(t, c.reply(3)); got != "mesh_progress ok" {
		t.Fatalf("after the rotation: %q", got)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if last := g.calls[len(g.calls)-1]; last != "tools/call token-B" {
		t.Fatalf("last call %q, want tools/call on token-B", last)
	}
	if g.inits != 1 {
		t.Fatalf("initialized %d times, want 1", g.inits)
	}
}

func TestRelayRetriesA401OnTheFileReadAgain(t *testing.T) {
	g, srv := newGateway(t, "token-A")
	file := filepath.Join(t.TempDir(), "token")
	writeToken(t, file, "token-A")
	// The first request after the gateway moves to B still carries A;
	// the file is rewritten between that try and the retry.
	var once sync.Once
	r := &Relay{URL: srv.URL, TokenFile: file, Stderr: io.Discard, HTTP: &http.Client{
		Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err == nil && resp.StatusCode == http.StatusUnauthorized {
				once.Do(func() { writeToken(t, file, "token-B") })
			}
			return resp, err
		}),
	}}
	c := startRelay(t, r)
	c.handshake()
	g.seen(2)
	g.accept("token-B")
	c.send(fmt.Sprintf(progressCall, 2))
	if got := toolText(t, c.reply(2)); got != "mesh_progress ok" {
		t.Fatalf("after a 401: %q", got)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRelayReinitializesAForgottenSession(t *testing.T) {
	g, srv := newGateway(t, "token-A")
	file := filepath.Join(t.TempDir(), "token")
	writeToken(t, file, "token-A")
	c := startRelay(t, &Relay{URL: srv.URL, TokenFile: file, Stderr: io.Discard})

	c.handshake()
	g.seen(2) // the initialized notification landed on the first session
	g.forget()

	c.send(fmt.Sprintf(progressCall, 2))
	if got := toolText(t, c.reply(2)); got != "mesh_progress ok" {
		t.Fatalf("after the session was dropped: %q", got)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inits != 2 {
		t.Fatalf("initialized %d times, want 2", g.inits)
	}
	want := []string{
		"initialize token-A", "notifications/initialized token-A",
		"tools/call token-A",
		"initialize token-A", "notifications/initialized token-A",
		"tools/call token-A",
	}
	if strings.Join(g.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(g.calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestRelayAnswersAnEmptyTokenFileWithAnError(t *testing.T) {
	_, srv := newGateway(t, "token-A")
	file := filepath.Join(t.TempDir(), "token")
	writeToken(t, file, "  \n")
	var stderr bytes.Buffer
	c := startRelay(t, &Relay{URL: srv.URL, TokenFile: file, Stderr: &stderr})

	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	m := c.reply(1)
	e, _ := m["error"].(map[string]any)
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, file) || !strings.Contains(msg, "empty") {
		t.Fatalf("error %v, want one naming %s as empty", m, file)
	}
}

func TestRelayAnswersAMissingTokenFileWithAnError(t *testing.T) {
	_, srv := newGateway(t, "token-A")
	file := filepath.Join(t.TempDir(), "absent")
	c := startRelay(t, &Relay{URL: srv.URL, TokenFile: file, Stderr: io.Discard})

	c.send(`{"jsonrpc":"2.0","id":"a","method":"tools/list"}`)
	timeout := time.After(5 * time.Second)
	select {
	case m := <-c.out:
		e, _ := m["error"].(map[string]any)
		msg, _ := e["message"].(string)
		if m["id"] != "a" || !strings.Contains(msg, file) {
			t.Fatalf("got %v, want an error for id a naming %s", m, file)
		}
	case <-timeout:
		t.Fatal("no answer")
	}
}

func TestRelayRelaysEveryEventStreamMessage(t *testing.T) {
	_, srv := newGateway(t, "token-A")
	file := filepath.Join(t.TempDir(), "token")
	writeToken(t, file, "token-A")
	c := startRelay(t, &Relay{URL: srv.URL, TokenFile: file, Stderr: io.Discard})
	c.handshake()
	c.send(fmt.Sprintf(progressCall, 2))
	first := <-c.out
	if first["method"] != "notifications/progress" {
		t.Fatalf("first message %v, want the server's progress notification", first)
	}
	if got := toolText(t, c.reply(2)); got != "mesh_progress ok" {
		t.Fatalf("%q", got)
	}
}
