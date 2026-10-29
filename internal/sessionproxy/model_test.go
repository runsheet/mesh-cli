package sessionproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	realModelToken = "sk-ant-oat01-real-held-by-the-node"
	dummyToken     = "mesh-relay-dummy"
)

// modelRig is a proxy whose model route reaches a fake upstream, served
// on a loopback port as a host session reaches it.
type modelRig struct {
	p     *Proxy
	base  string // the route's root, with its capability
	mu    sync.Mutex
	calls []ModelCall
	fresh int
}

func newModelRig(t *testing.T, upstream http.Handler, cred func(fresh bool) ModelCredential) *modelRig {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	g := &modelRig{}
	g.p = New(nil)
	g.p.Model = &Model{
		Upstreams: map[string]string{"anthropic": up.URL, "openai": up.URL + "/openai"},
		Credential: func(_ context.Context, provider string, fresh bool) (ModelCredential, error) {
			if provider != "anthropic" && provider != "openai" {
				return ModelCredential{}, errors.New("not the lease's provider")
			}
			g.mu.Lock()
			if fresh {
				g.fresh++
			}
			g.mu.Unlock()
			return cred(fresh), nil
		},
		Log: func(c ModelCall) {
			g.mu.Lock()
			g.calls = append(g.calls, c)
			g.mu.Unlock()
		},
	}
	base, err := g.p.ListenModel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.p.Close() })
	g.base = base
	return g
}

func bearer(fresh bool) ModelCredential {
	return ModelCredential{Header: "Authorization", Value: "Bearer " + realModelToken}
}

func (g *modelRig) logged() []ModelCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]ModelCall(nil), g.calls...)
}

// The session's request reaches the upstream with the lease's real
// credential in place of whatever the session sent — its dummy bearer
// and any x-api-key are dropped — and with its own headers and body
// otherwise intact; the answer comes back as the upstream gave it, and
// the call is logged with its status and size but no credential.
func TestModelRouteSwapsTheCredential(t *testing.T) {
	for _, c := range []struct {
		name   string
		cred   ModelCredential
		header string
	}{
		{"subscription", ModelCredential{Header: "Authorization", Value: "Bearer " + realModelToken}, "Authorization"},
		{"api key", ModelCredential{Header: "X-Api-Key", Value: realModelToken}, "X-Api-Key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got *http.Request
			var body string
			g := newModelRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Clone(context.Background())
				b, _ := io.ReadAll(r.Body)
				body = string(b)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Request-Id", "req_1")
				io.WriteString(w, `{"type":"message"}`)
			}), func(bool) ModelCredential { return c.cred })
			req, _ := http.NewRequest("POST", g.base+"/model/anthropic/v1/messages?beta=true", strings.NewReader(`{"model":"haiku"}`))
			req.Header.Set("Authorization", "Bearer "+dummyToken)
			req.Header.Set("X-Api-Key", dummyToken)
			req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
			req.Header.Set("Anthropic-Version", "2023-06-01")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			answer, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(answer) != `{"type":"message"}` || resp.Header.Get("Request-Id") != "req_1" {
				t.Fatalf("%d %s %v", resp.StatusCode, answer, resp.Header)
			}
			if got == nil || got.URL.Path != "/v1/messages" || got.URL.RawQuery != "beta=true" || body != `{"model":"haiku"}` {
				t.Fatalf("upstream saw %v %q", got, body)
			}
			want := c.cred.Value
			if got.Header.Get(c.header) != want {
				t.Fatalf("upstream %s %q", c.header, got.Header.Get(c.header))
			}
			for k, vs := range got.Header {
				for _, v := range vs {
					if strings.Contains(v, dummyToken) {
						t.Fatalf("the session's dummy left the relay in %s", k)
					}
				}
			}
			if c.header == "X-Api-Key" && got.Header.Get("Authorization") != "" {
				t.Fatalf("an api key's request carried Authorization %q", got.Header.Get("Authorization"))
			}
			if got.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || got.Header.Get("Anthropic-Version") != "2023-06-01" {
				t.Fatalf("the session's headers were not forwarded: %v", got.Header)
			}
			calls := g.logged()
			if len(calls) != 1 || calls[0].Status != 200 || calls[0].Bytes != int64(len(answer)) || calls[0].Path != "/v1/messages" || calls[0].Provider != "anthropic" {
				t.Fatalf("logged %+v", calls)
			}
		})
	}
}

// An event stream reaches the session a piece at a time, as the
// upstream writes it: the first event arrives while the upstream still
// holds the second.
func TestModelRouteStreamsUnbuffered(t *testing.T) {
	release := make(chan struct{})
	g := newModelRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	}), bearer)
	resp, err := http.Post(g.base+"/model/anthropic/v1/messages", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make(chan string, 1)
	rd := bufio.NewReader(resp.Body)
	go func() {
		line, _ := rd.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "event: message_start\n" {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first event did not arrive before the stream ended: the relay buffers")
	}
	close(release)
	rest, _ := io.ReadAll(rd)
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("the rest %q", rest)
	}
}

// An upstream's 401 has the credential read again and the request asked
// once more with the fresh one, body and all; the session sees only the
// second answer.
func TestModelRouteRefreshesOn401(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	g := newModelRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization")+"|"+string(b))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error"}}`)
			return
		}
		io.WriteString(w, "ok")
	}), func(fresh bool) ModelCredential {
		if fresh {
			return ModelCredential{Header: "Authorization", Value: "Bearer fresh"}
		}
		return ModelCredential{Header: "Authorization", Value: "Bearer stale"}
	})
	resp, err := http.Post(g.base+"/model/anthropic/v1/messages", "application/json", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "ok" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if strings.Join(seen, ",") != "Bearer stale|hello,Bearer fresh|hello" || g.fresh != 1 {
		t.Fatalf("upstream saw %v, fresh reads %d", seen, g.fresh)
	}
	if calls := g.logged(); len(calls) != 1 || !calls[0].Refreshed || calls[0].Status != 200 {
		t.Fatalf("logged %+v", calls)
	}
}

// A second 401 is the session's answer: the relay asks again only once.
func TestModelRouteRefreshesOnce(t *testing.T) {
	n := 0
	g := newModelRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusUnauthorized)
	}), bearer)
	resp, err := http.Post(g.base+"/model/anthropic/v1/messages", "application/json", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || n != 2 {
		t.Fatalf("%d after %d tries", resp.StatusCode, n)
	}
}

// The loopback port answers only under the lease's capability and only
// the model route: no other path, no MCP server, no git.
func TestListenModelNeedsTheCapability(t *testing.T) {
	g := newModelRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }), bearer)
	root := g.base[:strings.LastIndex(g.base, "/")]
	for _, u := range []string{
		root + "/model/anthropic/v1/messages",
		root + "/00000000000000000000000000000000/model/anthropic/v1/messages",
		g.base + "/mcp/x",
		g.base + "/git/acme/repo.git/info/refs",
		g.base + "/latchkey/github/user",
	} {
		resp, err := http.Post(u, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s answered %d", u, resp.StatusCode)
		}
	}
	resp, err := http.Post(g.base+"/model/anthropic/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the capability answered %d", resp.StatusCode)
	}
	g.p.Close()
	if _, err := http.Post(g.base+"/model/anthropic/v1/messages", "application/json", strings.NewReader("{}")); err == nil {
		t.Fatal("the model port still answers after Close")
	}
}

// A provider the relay does not reach, or the credential refusing it, is
// refused by the relay itself and never reaches an upstream.
func TestModelRouteRefusesAnUnknownProvider(t *testing.T) {
	reached := false
	g := newModelRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }), bearer)
	for _, path := range []string{"/model/gemini/v1/x", "/model/anthropic/../openai/v1/x"} {
		resp, err := http.Post(g.base+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 4 {
			t.Fatalf("%s answered %d", path, resp.StatusCode)
		}
	}
	if reached {
		t.Fatal("a refused call reached the upstream")
	}
	p := New(nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/model/anthropic/v1/messages", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a proxy with no model route answered %d", rec.Code)
	}
}
