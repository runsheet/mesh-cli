package mcprelay

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// socketClient is an HTTP client that dials sock whatever the URL says,
// through its directory's descriptor as the pod's relay reaches it from
// its directory: the path may be past sun_path's limit.
func socketClient(t *testing.T, sock string) *http.Client {
	dir, err := os.Open(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dir.Close() })
	addr := fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(sock))
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", addr)
	}}}
}

// The endpoint keeps the bearer on the node: a client that sends no
// Authorization — or one of its own — reaches /mcp on the token file's
// bearer of the moment, the session id and an event stream carried both
// ways; the socket is 0600 and holds nothing, and a missing token is the
// endpoint's 502, never a call without a bearer.
func TestRelayBearerStaysOnTheNode(t *testing.T) {
	g, srv := newGateway(t, "t1")
	dir := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "runsheet.token")
	if err := os.WriteFile(tokenFile, []byte("t1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a path past sun_path's limit still serves
	long := filepath.Join(dir, strings.Repeat("d", max(1, 110-len(dir))))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(long, "runsheet.sock")
	ep, err := Serve(sock, srv.URL+"/mcp", tokenFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if fi, err := os.Lstat(sock); err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket %v, %v", fi, err)
	}

	// relaying through the endpoint is a mesh mcp-relay whose server is
	// the socket: no token file in the client's hands
	c := socketClient(t, sock)
	post := func(body, session, auth string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://runsheet/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`, "", "")
	session := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if resp.StatusCode != 200 || session == "" {
		t.Fatalf("initialize: %d session %q", resp.StatusCode, session)
	}
	resp = post(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`, session, "Bearer the-pods-own")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(string(b), `"id":2`) {
		t.Fatalf("tools/call: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}

	// the node rotates the file; the next call carries the new bearer
	g.accept("t2")
	if err := os.WriteFile(tokenFile, []byte("t2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp = post(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`, session, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("after rotation: %d", resp.StatusCode)
	}
	g.mu.Lock()
	calls := append([]string(nil), g.calls...)
	g.mu.Unlock()
	want := []string{"initialize t1", "tools/call t1", "tools/list t2"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("the gateway saw %q, want %q", calls, want)
	}

	// no token, no call
	os.Remove(tokenFile)
	resp = post(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, session, "")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), "token file") {
		t.Fatalf("missing token: %d %s", resp.StatusCode, b)
	}
	g.mu.Lock()
	upstream := len(g.calls)
	g.mu.Unlock()
	if upstream != 3 {
		t.Fatalf("a call went upstream with no token: %d calls", upstream)
	}

	if err := ep.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left after Close: %v", err)
	}
}

// A socket a node before this one left is served again.
func TestEndpointReplacesAStaleSocket(t *testing.T) {
	_, srv := newGateway(t, "t1")
	dir, err := os.MkdirTemp("", "ep")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "runsheet.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	ep, err := Serve(sock, srv.URL+"/mcp", filepath.Join(dir, "token"), nil)
	if err != nil {
		t.Fatalf("serve over a stale socket: %v", err)
	}
	ep.Close()
	if _, err := Serve(sock, "not a url", "", nil); err == nil {
		t.Fatal("served a relative url")
	}
}
