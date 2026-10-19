package sessionproxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	toolSecret = "tool-secret-0123456789"
	bearerOld  = "rs_bearer_old_abcdef"
	bearerNew  = "rs_bearer_new_ghijkl"
)

// seen is what an upstream was sent.
type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (s *seen) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r.Clone(context.Background()))
}

func (s *seen) all() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.reqs...)
}

// listen starts p on a socket in a temp dir and returns a client that
// dials it.
func listen(t *testing.T, p *Proxy) (*http.Client, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "mcp.sock")
	if err := p.Listen(sock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	return c, sock
}

func post(t *testing.T, c *http.Client, server string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://proxy"+Prefix+server, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuf) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format+"\n", args...)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

func TestProxyRemoteGetsHeldSecretAndDropsClientAuthorization(t *testing.T) {
	var s seen
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.add(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer up.Close()
	p := New(map[string]Upstream{"tool": Remote{URL: up.URL, Headers: map[string]string{"X-Api-Key": toolSecret}}})
	c, _ := listen(t, p)

	resp, _ := post(t, c, "tool", map[string]string{
		"Authorization": "Bearer from-the-agent",
		"X-Api-Key":     "agent-chosen",
		"Cookie":        "a=b",
	}, initialize)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	reqs := s.all()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests", len(reqs))
	}
	h := reqs[0].Header
	if got := h.Get("X-Api-Key"); got != toolSecret {
		t.Errorf("X-Api-Key = %q, want the held secret", got)
	}
	if got := h.Values("X-Api-Key"); len(got) != 1 {
		t.Errorf("X-Api-Key values = %q, want one", got)
	}
	if got := h.Get("Authorization"); got != "" {
		t.Errorf("Authorization reached the upstream: %q", got)
	}
	if got := h.Get("Cookie"); got != "" {
		t.Errorf("Cookie reached the upstream: %q", got)
	}
}

func TestProxyRunsheetBearerAskedPerRequestAndOnceMoreAfter401(t *testing.T) {
	var s seen
	var mu sync.Mutex
	accept := bearerNew
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.add(r)
		mu.Lock()
		ok := r.Header.Get("Authorization") == "Bearer "+accept
		mu.Unlock()
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer up.Close()

	var asks int
	tokens := []string{bearerOld, bearerNew}
	bearer := func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		tok := tokens[min(asks, len(tokens)-1)]
		asks++
		return tok, nil
	}
	p := New(map[string]Upstream{"runsheet": Runsheet{URL: up.URL, Bearer: bearer}})
	c, _ := listen(t, p)

	resp, body := post(t, c, "runsheet", map[string]string{"Authorization": "Bearer agent"}, initialize)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	reqs := s.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream saw %d requests, want the 401 and one retry", len(reqs))
	}
	if got := reqs[0].Header.Get("Authorization"); got != "Bearer "+bearerOld {
		t.Errorf("first Authorization = %q", got)
	}
	if got := reqs[1].Header.Get("Authorization"); got != "Bearer "+bearerNew {
		t.Errorf("retry Authorization = %q", got)
	}
	if asks != 2 {
		t.Errorf("Bearer asked %d times, want 2", asks)
	}

	// Still refused after the retry: the 401 is the answer, not a loop.
	mu.Lock()
	accept = "nobody"
	mu.Unlock()
	resp, _ = post(t, c, "runsheet", nil, initialize)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
	if got := len(s.all()); got != 4 {
		t.Errorf("upstream saw %d requests in all, want 4", got)
	}
}

func TestProxyUnknownServerIs404(t *testing.T) {
	p := New(map[string]Upstream{"runsheet": Runsheet{URL: "http://127.0.0.1:1", Bearer: func(context.Context) (string, error) { return bearerNew, nil }}})
	c, _ := listen(t, p)
	for _, server := range []string{"nope", "", "runsheet/extra"} {
		resp, body := post(t, c, server, nil, initialize)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%q: status %d, want 404", server, resp.StatusCode)
		}
		if !strings.Contains(body, `"jsonrpc":"2.0"`) || !strings.Contains(body, `"error"`) {
			t.Errorf("%q: body %s is not a JSON-RPC error", server, body)
		}
	}
}

func TestProxyCarriesSessionIdAndStreamsEventStream(t *testing.T) {
	var s seen
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.add(r)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"initialize"`) {
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n")
	}))
	defer up.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	p := New(map[string]Upstream{"runsheet": Runsheet{URL: up.URL, Bearer: func(context.Context) (string, error) { return bearerNew, nil }}})
	c, _ := listen(t, p)

	resp, _ := post(t, c, "runsheet", nil, initialize)
	sess := resp.Header.Get("Mcp-Session-Id")
	if sess != "sess-1" {
		t.Fatalf("Mcp-Session-Id back = %q", sess)
	}

	req, _ := http.NewRequest(http.MethodPost, "http://proxy"+Prefix+"runsheet",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sess)
	stream, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if ct := stream.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type %q", ct)
	}
	// The first event arrives while the upstream still holds the second.
	first := make(chan string, 1)
	rd := bufio.NewReader(stream.Body)
	go func() {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				first <- ""
				return
			}
			if strings.HasPrefix(line, "data:") {
				first <- line
				return
			}
		}
	}()
	select {
	case line := <-first:
		if !strings.Contains(line, "notifications/progress") {
			t.Fatalf("first event %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event was not streamed before the reply ended")
	}
	close(release)
	rest, _ := io.ReadAll(rd)
	if !strings.Contains(string(rest), `"id":2`) {
		t.Errorf("the rest of the stream: %q", rest)
	}
	reqs := s.all()
	if got := reqs[len(reqs)-1].Header.Get("Mcp-Session-Id"); got != sess {
		t.Errorf("upstream got Mcp-Session-Id %q, want %q", got, sess)
	}
}

func TestProxyNeverRepliesOrLogsTheSecretOrBearer(t *testing.T) {
	// Upstreams that echo what they were sent, in a reply, an error and
	// a stream.
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization") + " " + r.Header.Get("X-Api-Key")
		switch r.URL.Query().Get("as") {
		case "error":
			http.Error(w, "refused "+got, http.StatusForbidden)
		case "stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"echo\":%q}\n\n", got)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"echo":%q}}`, got)
		}
	})
	up := httptest.NewServer(echo)
	defer up.Close()
	var logs logBuf
	p := New(nil)
	p.Logf = logs.logf
	for _, as := range []string{"json", "error", "stream"} {
		p.Add("tool-"+as, Remote{URL: up.URL + "?as=" + as, Headers: map[string]string{"X-Api-Key": toolSecret, "Authorization": "Token " + toolSecret + "-auth"}})
		p.Add("runsheet-"+as, Runsheet{URL: up.URL + "?as=" + as, Bearer: func(context.Context) (string, error) { return bearerNew, nil }})
	}
	p.Add("runsheet-broken", Runsheet{URL: up.URL, Bearer: func(context.Context) (string, error) {
		return "", fmt.Errorf("token file holds %s but it expired", bearerNew)
	}})
	p.Add("tool-down", Remote{URL: "http://127.0.0.1:1/?k=" + toolSecret, Headers: map[string]string{"X-Api-Key": toolSecret}})
	c, _ := listen(t, p)

	var replies []string
	for _, server := range []string{"tool-json", "tool-error", "tool-stream", "runsheet-json", "runsheet-error", "runsheet-stream", "runsheet-broken", "tool-down"} {
		_, body := post(t, c, server, nil, initialize)
		replies = append(replies, body)
	}
	all := strings.Join(replies, "\n")
	if !strings.Contains(all, "[redacted]") {
		t.Fatalf("no reply was masked; the echoes did not run: %s", all)
	}
	for _, secret := range []string{toolSecret, bearerNew} {
		if strings.Contains(all, secret) {
			t.Errorf("a reply holds %q:\n%s", secret, all)
		}
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs.String())
		}
	}
	if logs.String() == "" {
		t.Error("nothing was logged; the error paths did not run")
	}
}

func TestProxySocketIs0600AndGoneAfterClose(t *testing.T) {
	p := New(nil)
	_, sock := listen(t, p)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket: %v", sock, fi.Mode())
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("socket mode %o, want 600", got)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still there after Close: %v", err)
	}
}
