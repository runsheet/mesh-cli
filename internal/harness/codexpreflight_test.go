package harness

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// preflightCodex is a `codex` whose `mcp get` records its environment
// and CODEX_HOME's config.toml in out and exits with code.
func preflightCodex(t *testing.T, out string, code int) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"echo \"$@\" > " + out + "/args\n" +
		"env > " + out + "/env\n" +
		"cp \"$CODEX_HOME/config.toml\" " + out + "/config.toml\n" +
		"[ " + itoa(code) + " = 0 ] || echo 'Error: No MCP server named prologue found.' >&2\n" +
		"exit " + itoa(code) + "\n"
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func itoa(n int) string { return string(rune('0' + n)) }

// mcpServer answers an MCP client over streamable HTTP with tools, when
// the Authorization is want; else status with body.
func mcpServer(t *testing.T, want string, status int, body string, tools int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.WriteHeader(status)
			io.WriteString(w, body)
			return
		}
		var msg struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&msg)
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s-1")
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"mesh","version":"1"}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "s-1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			list := make([]map[string]any, tools)
			for i := range list {
				list[i] = map[string]any{"name": "mesh_document", "inputSchema": map[string]any{"type": "object"}}
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"tools": list}})
			// as an event stream, as the gateway may answer
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message\ndata: "+string(b)+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func builtin(url, auth string) map[string]any {
	return map[string]any{BuiltinMCP: map[string]any{"type": "http", "url": url, "headers": map[string]string{"Authorization": auth}}}
}

// A tools/list with one tool, on the header the rendered config names:
// nil, after `codex mcp get prologue` in the session's home.
func TestCodexPreflightPasses(t *testing.T) {
	out := t.TempDir()
	srv := mcpServer(t, "Bearer rs_node", http.StatusUnauthorized, "", 1)
	c := &Codex{Binary: preflightCodex(t, out, 0), HomeRoot: t.TempDir(), Account: CodexAccount{Name: "key", APIKeyEnv: "TEST_PREFLIGHT_KEY"}}
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	job := Job{Workspace: t.TempDir(), MCPServers: builtin(srv.URL+"/mcp", "Bearer rs_node")}
	if err := c.Preflight(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(out, "args"))); got != "mcp get prologue" {
		t.Fatalf("args %q", got)
	}
	if !strings.Contains(readFile(t, filepath.Join(out, "config.toml")), `env_http_headers = { "Authorization" = "MESH_CODEX_MCP_0_TOKEN" }`) {
		t.Fatal("codex mcp get did not see the rendered config")
	}
	if env := envOf(t, filepath.Join(out, "env")); env["MESH_CODEX_MCP_0_TOKEN"][0] != "Bearer rs_node" || env["CODEX_API_KEY"][0] != "sk-test" {
		t.Fatalf("env %v", env)
	}
}

// A 401: `refuses auth` with the status and the body, and no secret.
func TestCodexPreflightRefusesAuth(t *testing.T) {
	srv := mcpServer(t, "Bearer rs_other", http.StatusUnauthorized, `{"error":"bad token Bearer rs_node"}`, 1)
	c := &Codex{Binary: preflightCodex(t, t.TempDir(), 0), HomeRoot: t.TempDir(), Account: CodexAccount{Name: "key", APIKeyEnv: "TEST_PREFLIGHT_KEY"}}
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	err := c.Preflight(context.Background(), Job{Workspace: t.TempDir(), MCPServers: builtin(srv.URL, "Bearer rs_node")})
	if err == nil || !strings.Contains(err.Error(), "refuses auth") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "rs_node") {
		t.Fatalf("the error carries the secret: %v", err)
	}
}

// A closed port: `unreachable`.
func TestCodexPreflightUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	c := &Codex{Binary: preflightCodex(t, t.TempDir(), 0), HomeRoot: t.TempDir(), Account: CodexAccount{Name: "key", APIKeyEnv: "TEST_PREFLIGHT_KEY"}}
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	err = c.Preflight(context.Background(), Job{Workspace: t.TempDir(), MCPServers: builtin("http://"+addr+"/mcp", "Bearer rs_node")})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err %v", err)
	}
}

// An empty header value, no tools, and a `codex mcp get` that fails are
// each an error that says so.
func TestCodexPreflightOtherFailures(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	acct := CodexAccount{Name: "key", APIKeyEnv: "TEST_PREFLIGHT_KEY"}
	srv := mcpServer(t, "", http.StatusUnauthorized, "", 1)
	c := &Codex{Binary: preflightCodex(t, t.TempDir(), 0), HomeRoot: t.TempDir(), Account: acct}
	err := c.Preflight(context.Background(), Job{Workspace: t.TempDir(), MCPServers: builtin(srv.URL, "")})
	if err == nil || !strings.Contains(err.Error(), "MESH_CODEX_MCP_0_TOKEN for prologue's Authorization header is empty") {
		t.Fatalf("empty: %v", err)
	}

	srv = mcpServer(t, "Bearer rs_node", http.StatusUnauthorized, "", 0)
	err = c.Preflight(context.Background(), Job{Workspace: t.TempDir(), MCPServers: builtin(srv.URL, "Bearer rs_node")})
	if err == nil || !strings.Contains(err.Error(), "lists no tools") {
		t.Fatalf("no tools: %v", err)
	}

	c.Binary = preflightCodex(t, t.TempDir(), 1)
	err = c.Preflight(context.Background(), Job{Workspace: t.TempDir(), MCPServers: builtin(srv.URL, "Bearer rs_node")})
	if err == nil || !strings.Contains(err.Error(), "codex mcp get prologue: Error: No MCP server named prologue found.") {
		t.Fatalf("mcp get: %v", err)
	}

	// no built-in: nothing to check
	if err := c.Preflight(context.Background(), Job{Workspace: t.TempDir()}); err != nil {
		t.Fatalf("no built-in: %v", err)
	}
}
