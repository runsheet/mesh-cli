package harness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/node"
)

// An evaluation session on either family gets a context tool it can
// reach (goal evaluation-reports-no-verdict, item 2): the job is built
// as Evaluator.Evaluate builds it — SessionTools.Apply with
// MeshContextMCP of the gateway and the node's token — and each
// family's rendering is asserted as text, then answered by a fake MCP
// server at the rendered url, on the rendered header.

// fakeGateway is the gateway's /mcp: initialize, notifications/
// initialized and tools/list over streamable HTTP, recording every
// Authorization header it was sent. A request to any other path is a
// 404.
type fakeGateway struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
	calls []string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		var msg struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&msg)
		g.mu.Lock()
		g.auths = append(g.auths, r.Header.Get("Authorization"))
		g.calls = append(g.calls, msg.Method)
		g.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer rs_node" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s-1")
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"mesh","version":"1"}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+itoa(msg.ID)+`,"result":{"tools":[{"name":"mesh_document","inputSchema":{"type":"object"}}]}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (g *fakeGateway) seen() ([]string, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auths...), append([]string(nil), g.calls...)
}

// evaluationJob is the job Evaluator.Evaluate hands its harness: the
// evaluate kind, the workspace, and the servers SessionTools.Apply puts
// on it from MeshContextMCP (no console tools served).
func evaluationJob(t *testing.T, gateway string) harness.Job {
	t.Helper()
	job := harness.Job{Kind: harness.KindEvaluate, Workspace: t.TempDir(), Prompt: "evaluate", Model: "gpt-6-sol"}
	if _, err := (node.SessionTools{}).Apply(&job, node.MeshContextHTTP(gateway, "rs_node")); err != nil {
		t.Fatal(err)
	}
	return job
}

// Codex: the rendered config.toml names mesh-context at the gateway's
// /mcp, its Authorization from MESH_CODEX_MCP_0_TOKEN, approved; that
// variable holds the node's token in the session's environment; and
// the preflight passes against the fake, which was sent exactly the
// rendered header.
func TestEvaluationOnCodexReachesTheContextTool(t *testing.T) {
	g := newFakeGateway(t)
	job := evaluationJob(t, g.URL+"/")

	toml, env, err := harness.CodexMCPConfig(job.MCPServers, job.Env)
	if err != nil {
		t.Fatal(err)
	}
	want := "[mcp_servers.mesh-context]\n" +
		"url = \"" + g.URL + "/mcp\"\n" +
		"env_http_headers = { \"Authorization\" = \"MESH_CODEX_MCP_0_TOKEN\" }\n" +
		"default_tools_approval_mode = \"approve\"\n"
	if toml != want {
		t.Fatalf("config.toml:\n%s\nwant:\n%s", toml, want)
	}
	if len(env) != 1 || env[0] != "MESH_CODEX_MCP_0_TOKEN=Bearer rs_node" {
		t.Fatalf("env %q", env)
	}

	// a `codex` whose `mcp get` records the session's env and the
	// config.toml it would read, and whose sandbox probe passes
	out := t.TempDir()
	script := "#!/bin/sh\n" +
		"[ \"$1\" = sandbox ] && exit 0\n" +
		"echo \"$@\" > " + out + "/args\n" +
		"env > " + out + "/env\n" +
		"cp \"$CODEX_HOME/config.toml\" " + out + "/config.toml\n"
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_EVAL_CODEX_KEY", "sk-test")
	c := &harness.Codex{Binary: bin, HomeRoot: t.TempDir(), Account: harness.CodexAccount{Name: "key", APIKeyEnv: "TEST_EVAL_CODEX_KEY"}}
	if err := c.Preflight(context.Background(), job); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if got := strings.TrimSpace(read(t, filepath.Join(out, "args"))); got != "mcp get mesh-context" {
		t.Fatalf("args %q", got)
	}
	if got := read(t, filepath.Join(out, "config.toml")); !strings.Contains(got, want) {
		t.Fatalf("the session's config.toml:\n%s\nwant it to carry:\n%s", got, want)
	}
	if !strings.Contains("\n"+read(t, filepath.Join(out, "env")), "\nMESH_CODEX_MCP_0_TOKEN=Bearer rs_node\n") {
		t.Fatal("the session's env has no MESH_CODEX_MCP_0_TOKEN with the node's token")
	}
	auths, calls := g.seen()
	if strings.Join(calls, ",") != "initialize,notifications/initialized,tools/list" {
		t.Fatalf("calls %q", calls)
	}
	for _, a := range auths {
		if a != "Bearer rs_node" {
			t.Fatalf("the fake was sent Authorization %q, want the rendered Bearer rs_node", a)
		}
	}
}

// Claude: the mcp.json names mesh-context at the gateway's /mcp with
// the node's bearer, --allowedTools lets its tools run (inline, and as
// the sandbox's file), and a tools/list POST to the rendered url on the
// rendered header is answered.
func TestEvaluationOnClaudeReachesTheContextTool(t *testing.T) {
	g := newFakeGateway(t)
	job := evaluationJob(t, g.URL)

	args := harness.MCPArgs(job.MCPServers)
	if len(args) != 4 || args[0] != "--mcp-config" || args[2] != "--allowedTools" {
		t.Fatalf("args %q", args)
	}
	want := `{"mcpServers":{"mesh-context":{"headers":{"Authorization":"Bearer rs_node"},"type":"http","url":"` + g.URL + `/mcp"}}}`
	if args[1] != want {
		t.Fatalf("mcp.json:\n%s\nwant:\n%s", args[1], want)
	}
	if args[3] != "mcp__mesh-context__*" {
		t.Fatalf("--allowedTools %q", args[3])
	}

	c := &harness.ClaudeCode{}
	sargs := strings.Join(c.SandboxArgs(job, "/home/session/mcp.json"), " ")
	if !strings.Contains(sargs, "--mcp-config /home/session/mcp.json --allowedTools mcp__mesh-context__*") {
		t.Fatalf("sandbox args %q", sargs)
	}
	if strings.Contains(sargs, "rs_node") {
		t.Fatalf("the sandbox args carry the bearer: %q", sargs)
	}

	// the fake at the rendered url, on the rendered header
	var cfg struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[1]), &cfg); err != nil {
		t.Fatal(err)
	}
	srv := cfg.MCPServers["mesh-context"]
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	for k, v := range srv.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"mesh_document"`) {
		t.Fatalf("tools/list: %d %s", resp.StatusCode, body)
	}
	if auths, _ := g.seen(); len(auths) != 1 || auths[0] != "Bearer rs_node" {
		t.Fatalf("the fake was sent Authorization %q", auths)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
