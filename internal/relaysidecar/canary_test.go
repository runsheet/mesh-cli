package relaysidecar

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

const sidecarLeak = "injected-value-0123456789"

// nodeCanary is the node's canary as these tests see it: it finds
// sidecarLeak and redacts the product's name.
type nodeCanary struct {
	mu   sync.Mutex
	seen []string
}

func (c *nodeCanary) Scan(where, toward string, b []byte) error {
	if !bytes.Contains(b, []byte(sidecarLeak)) {
		return nil
	}
	c.mu.Lock()
	c.seen = append(c.seen, where+" toward "+toward)
	c.mu.Unlock()
	return errors.New(sessionproxy.CodeExposed)
}

func (c *nodeCanary) Redact(s string) string {
	return strings.ReplaceAll(s, "product", "[secret:sec_1]")
}

// The sidecar never holds the lease's injected values: it hands each
// product MCP call to the node to scan, refusing one the node finds a
// value in before it leaves, and each answer to the node to redact
// (mesh-specs gap 28 item 5).
func TestTheSidecarsCallsAreScannedOnTheNode(t *testing.T) {
	c := testCreds(t)
	c.Servers = []Server{{Name: "runsheet", URL: "https://product.example.com/mcp"}}
	c.Canary = true
	g := newRig(t, c, nil)
	canary := &nodeCanary{}
	g.node.Canary = canary

	code, body := g.post("/mcp/runsheet", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != 200 || strings.Contains(body, `"product"`) || !strings.Contains(body, "[secret:sec_1]") {
		t.Fatalf("the product route answered %d %s", code, body)
	}
	code, body = g.post("/mcp/runsheet", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x","arguments":{"v":"`+sidecarLeak+`"}}}`)
	if code != http.StatusForbidden || !strings.Contains(body, sessionproxy.CodeExposed) {
		t.Fatalf("a leaking call answered %d %s", code, body)
	}
	g.mu.Lock()
	calls := len(g.auth)
	g.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the product saw %d calls", calls)
	}
	canary.mu.Lock()
	defer canary.mu.Unlock()
	if len(canary.seen) != 1 || canary.seen[0] != "an MCP request's body toward the MCP server runsheet" {
		t.Fatalf("the node saw %q", canary.seen)
	}
}

// A node that cannot scan refuses the call; one that cannot redact
// withholds the answer.
func TestTheSidecarsCanaryFailsClosed(t *testing.T) {
	rc := &remoteCanary{node: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("the node is gone")
	})}, logf: t.Logf}
	if rc.Scan("w", "t", []byte("clean")) == nil {
		t.Fatal("a scan the node never made passed")
	}
	if got := rc.Redact("an answer"); got != unchecked {
		t.Fatalf("an answer the node never redacted: %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
