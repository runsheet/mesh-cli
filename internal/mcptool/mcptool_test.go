package mcptool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testHandler() *Handler {
	return &Handler{Name: "mesh-node", Tools: []Tool{{
		Name: "echo", Description: "says it back",
		Schema: map[string]any{"type": "object", "properties": map[string]any{"s": map[string]any{"type": "string"}}},
		Call: func(_ context.Context, args map[string]any) (string, error) {
			s, _ := args["s"].(string)
			if s == "" {
				return "", errors.New("refused: s is empty")
			}
			return "echo " + s, nil
		},
	}}}
}

func post(t *testing.T, srv *httptest.Server, msg any) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(msg)
	resp, err := http.Post(srv.URL+"/mcp/mesh-node", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// The handler speaks a tools server's streamable HTTP: initialize names
// the server, tools/list the tools, tools/call runs one — a refusal
// answered as the tool's isError result — a notification is a 202, and
// GET is 405.
func TestHandlerServesATools(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()
	resp, out := post(t, srv, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2024-11-05"}})
	if resp.StatusCode != 200 || out["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "mesh-node" || out["result"].(map[string]any)["protocolVersion"] != "2024-11-05" {
		t.Fatalf("initialize: %d %v", resp.StatusCode, out)
	}
	if resp, _ := post(t, srv, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a notification answered %d", resp.StatusCode)
	}
	_, out = post(t, srv, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if list := out["result"].(map[string]any)["tools"].([]any); len(list) != 1 || list[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools/list: %v", out)
	}
	_, out = post(t, srv, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "echo", "arguments": map[string]any{"s": "hi"}}})
	if res := out["result"].(map[string]any); res["isError"] == true || res["content"].([]any)[0].(map[string]any)["text"] != "echo hi" {
		t.Fatalf("tools/call: %v", out)
	}
	_, out = post(t, srv, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "echo", "arguments": map[string]any{}}})
	if res := out["result"].(map[string]any); res["isError"] != true || res["content"].([]any)[0].(map[string]any)["text"] != "refused: s is empty" {
		t.Fatalf("a refused call: %v", out)
	}
	_, out = post(t, srv, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": map[string]any{"name": "nope"}})
	if out["error"] == nil {
		t.Fatalf("an unknown tool: %v", out)
	}
	resp, err := http.Get(srv.URL + "/mcp/mesh-node")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET answered %d", resp.StatusCode)
	}
}
