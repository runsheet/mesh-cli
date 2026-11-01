package sessionproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
)

const injected = "injected-value-0123456789"

// fakeCanary watches for injected, as the node's injected values do.
type fakeCanary struct {
	mu      sync.Mutex
	exposed []string
}

func (c *fakeCanary) Scan(where, toward string, b []byte) error {
	if !bytes.Contains(b, []byte(injected)) {
		return nil
	}
	c.mu.Lock()
	c.exposed = append(c.exposed, where+" toward "+toward)
	c.mu.Unlock()
	return errors.New(CodeExposed)
}

func (c *fakeCanary) Redact(s string) string {
	return strings.ReplaceAll(s, injected, "[secret:sec_1]")
}

func (c *fakeCanary) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.exposed...)
}

// An injected value in an MCP request's body is refused before it
// reaches the server, and one in a server's answer is redacted
// (mesh-specs gap 28 item 5).
func TestAnInjectedValueNeverCrossesTheMCPRoute(t *testing.T) {
	var s seen
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.add(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"text":"the key is `+injected+`"}}`)
	}))
	defer up.Close()
	p := New(map[string]Upstream{"tool": Remote{URL: up.URL}})
	canary := &fakeCanary{}
	p.Canary = canary
	c, _ := listen(t, p)

	resp, body := post(t, c, "tool", nil, initialize)
	if resp.StatusCode != 200 || strings.Contains(body, injected) || !strings.Contains(body, "[secret:sec_1]") {
		t.Fatalf("answer %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, c, "tool", nil, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x","arguments":{"v":"`+injected+`"}}}`)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, CodeExposed) {
		t.Fatalf("a leaking call answered %d %s", resp.StatusCode, body)
	}
	if n := len(s.all()); n != 1 {
		t.Fatalf("the server saw %d requests", n)
	}
	if got := canary.seen(); len(got) != 1 || got[0] != "an MCP request's body toward the MCP server tool" {
		t.Fatalf("exposures %q", got)
	}
}

// An injected value in a vendor API call's path, query or body — held
// or streamed — is refused before it reaches Latchkey, and one in the
// answer is redacted.
func TestAnInjectedValueNeverCrossesTheVendorAPIRoute(t *testing.T) {
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	lk := &fakeLatchkeyProxy{t: t, key: key, answer: func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"body":"`+injected+`"}`)
	}}
	up := httptest.NewServer(lk)
	defer up.Close()
	p := New(nil)
	var audit []APICall
	var mu sync.Mutex
	p.Latchkey = &Latchkey{URL: up.URL, Token: func(context.Context) (string, error) { return sessionTok, nil }, Key: key,
		Allow: func(string, string, string) bool { return true },
		Audit: func(c APICall) { mu.Lock(); audit = append(audit, c); mu.Unlock() }}
	canary := &fakeCanary{}
	p.Canary = canary
	c, _ := listen(t, p)
	call := func(method, path string, body io.Reader, length int64) (int, string) {
		req, _ := http.NewRequest(method, "http://relay"+path, body)
		if body != nil {
			req.ContentLength = length
		}
		resp, err := c.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, body := call("GET", "/latchkey/github/repos/acme/app/issues/1", nil, 0); st != 200 || strings.Contains(body, injected) || !strings.Contains(body, "[secret:sec_1]") {
		t.Fatalf("a read answered %d %s", st, body)
	}
	for name, tc := range map[string]struct {
		path string
		body string
	}{
		"query": {"/latchkey/github/search/code?q=" + injected, ""},
		"body":  {"/latchkey/github/repos/acme/app/issues", `{"body":"` + injected + `"}`},
	} {
		var rd io.Reader
		if tc.body != "" {
			rd = strings.NewReader(tc.body)
		}
		method := "GET"
		if rd != nil {
			method = "POST"
		}
		if st, body := call(method, tc.path, rd, int64(len(tc.body))); st != http.StatusForbidden || !strings.Contains(body, CodeExposed) {
			t.Fatalf("%s: answered %d %s", name, st, body)
		}
	}
	// a streamed body: the value straddles two of the scanner's chunks
	big := strings.Repeat("a", apiBodyMax+scanChunk-5) + injected + strings.Repeat("b", 1000)
	call("PUT", "/latchkey/github/repos/acme/app/contents/blob", strings.NewReader(big), int64(len(big)))
	for _, r := range lk.seen() {
		if r.Method != "GET" || strings.Contains(r.URL.RawQuery, injected) {
			if r.Method == "PUT" && r.ContentLength == int64(len(big)) {
				// it may have begun; Latchkey's read of it must have
				// failed short
				continue
			}
			t.Fatalf("Latchkey saw %s %s", r.Method, r.URL)
		}
	}
	got := canary.seen()
	slices.Sort(got)
	want := []string{"a vendor API call's body toward github's API", "a vendor API call's body toward github's API", "a vendor API call's path or query toward github's API"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("exposures\n%s", strings.Join(got, "\n"))
	}
	mu.Lock()
	defer mu.Unlock()
	refused := 0
	for _, a := range audit {
		if a.Outcome == "refused" {
			refused++
		}
	}
	if refused != 2 {
		t.Fatalf("audit %+v", audit)
	}
}

// A stream is handed on chunk by chunk, each scanned with the tail of
// the last, so a value split across two is found before the second goes.
func TestScanStreamFindsASplitValue(t *testing.T) {
	src := strings.Repeat("x", scanChunk-3) + injected + "tail"
	r := scanStream(strings.NewReader(src), &fakeCanary{}, "w", "t")
	got, err := io.ReadAll(r)
	if err == nil || len(got) != scanChunk {
		t.Fatalf("read %d bytes, err %v", len(got), err)
	}
	clean := strings.Repeat("y", 3*scanChunk+17)
	got, err = io.ReadAll(scanStream(strings.NewReader(clean), &fakeCanary{}, "w", "t"))
	if err != nil || string(got) != clean {
		t.Fatalf("a clean stream: %d bytes, %v", len(got), err)
	}
}
