package sessionproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Observe hears each JSON-RPC answer a remote upstream gives, beside the
// request it answers: a JSON body whole, an event stream event by event.
func TestObserveHearsEachAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, ctype, body string
		want              []string
	}{
		{"json", "application/json", `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`, []string{`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`}},
		{"stream", "text/event-stream", "event: message\ndata: {\"id\":1,\"a\":1}\n\ndata: {\"id\":2}\n\n", []string{`{"id":1,"a":1}`, `{"id":2}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				io.WriteString(w, tc.body)
			}))
			defer up.Close()
			p := New(map[string]Upstream{"prod": Remote{URL: up.URL}})
			var mu sync.Mutex
			var got []string
			var reqs []string
			p.Observe = func(server string, req, resp []byte) {
				mu.Lock()
				defer mu.Unlock()
				if server != "prod" {
					t.Errorf("server %q", server)
				}
				got = append(got, string(resp))
				reqs = append(reqs, string(req))
			}
			c, _ := listen(t, p)
			resp, err := c.Post("http://proxy/mcp/prod", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
			if err != nil {
				t.Fatal(err)
			}
			io.ReadAll(resp.Body)
			resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(got, "|") != strings.Join(tc.want, "|") || !strings.Contains(reqs[0], "tools/call") {
				t.Fatalf("observed %q for %q", got, reqs)
			}
		})
	}
}
