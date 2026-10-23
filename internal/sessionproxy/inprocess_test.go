package sessionproxy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/mcptool"
)

// An InProcess upstream is answered by its handler in this process, on
// the same path a session takes to every other server: a tools/call
// reaches the Go handler, and its refusal comes back as the tool's
// isError result.
func TestProxyServesAnInProcessUpstream(t *testing.T) {
	var got map[string]any
	h := &mcptool.Handler{Name: "mesh-node", Tools: []mcptool.Tool{{
		Name: "mesh_result", Schema: map[string]any{"type": "object"},
		Call: func(_ context.Context, args map[string]any) (string, error) {
			if args["answer"] == "" {
				return "", errors.New("refused: answer is empty")
			}
			got = args
			return "recorded", nil
		},
	}}}
	p := New(map[string]Upstream{"mesh-node": InProcess{Handler: h}})
	c, _ := listen(t, p)
	resp, body := post(t, c, "mesh-node", nil, initialize)
	if resp.StatusCode != 200 || !strings.Contains(body, `"name":"mesh-node"`) {
		t.Fatalf("initialize: %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, c, "mesh-node", nil, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"mesh_result","arguments":{"answer":""}}}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"isError":true`) || !strings.Contains(body, "refused: answer is empty") {
		t.Fatalf("a refused call: %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, c, "mesh-node", nil, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"mesh_result","arguments":{"answer":"hi"}}}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "recorded") || got["answer"] != "hi" {
		t.Fatalf("a call: %d %s %v", resp.StatusCode, body, got)
	}
	if resp, _ := post(t, c, "mesh-node", nil, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != 202 {
		t.Fatalf("a notification answered %d", resp.StatusCode)
	}
	if e := p.FirstError("mesh-node"); e != "" {
		t.Fatalf("an error was noted: %s", e)
	}
}
