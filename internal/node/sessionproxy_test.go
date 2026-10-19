package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
)

const (
	proxySessionBearer = "msb-7f3e1c9a55"
	proxyMachineBearer = "machine-bearer-77aa10"
)

// shortProxyRoot is an MCP root short enough for a unix socket's path
// under it (t.TempDir() can pass 108 bytes on macOS).
func shortProxyRoot(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// mcpUpstream is a fake HTTP MCP server answering every request with
// its own text, recording the Authorization each carried.
type mcpUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newMCPUpstream(t *testing.T, text string) *mcpUpstream {
	u := &mcpUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		u.mu.Unlock()
		var m map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m["id"] == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "up-1")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":%q}]}}`, m["id"], text)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *mcpUpstream) sawAuth(want string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, a := range u.auths {
		if a == want {
			return true
		}
	}
	return false
}

// fakeNpx puts an `npx` on PATH that runs a stdio MCP server answering
// every request with whether FS_API_KEY reached it — the local tool the
// proxy starts on the host.
func fakeNpx(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([^,}]*\).*/\1/p')
  [ -z "$id" ] && continue
  if [ "$FS_API_KEY" = "` + fsSecret + `" ]; then key=ok; else key=missing; fi
  printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"fs key %s"}]}}\n' "$id" "$key"
done
`
	if err := os.WriteFile(filepath.Join(bin, "npx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// callThrough sends an MCP client's initialize and a tools/call to
// server on socket, as the pod's shim would, and answers the call's
// reply body.
func callThrough(socket, server string) (string, error) {
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	session := ""
	var last string
	for i, msg := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agent","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"look","arguments":{}}}`,
	} {
		req, _ := http.NewRequest(http.MethodPost, "http://unix/mcp/"+server, strings.NewReader(msg))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := c.Do(req)
		if err != nil {
			return "", err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return "", fmt.Errorf("%s message %d: %d %s", server, i, resp.StatusCode, b)
		}
		if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
			session = s
		}
		last = string(b)
	}
	return last, nil
}

// A sandboxed build with a remote tool's secret, a local tool's secret
// and its own session bearer: every file under the pod's mounts, the
// pod's env and its --mcp-config hold none of them and no
// `mesh-session`, no runsheet.token is written, and a call through the
// session's socket reaches each fake upstream on its credential.
func TestAgentContainerHoldsNoCredential(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	fakeNpx(t)
	search := newMCPUpstream(t, "search answered")
	gatewayMCP := newMCPUpstream(t, "runsheet answered")

	f := seed(t)
	rt := remoteTool()
	rt["url"] = search.URL + "/mcp"
	f.SessionTools[item1] = []map[string]any{rt, localTool()}

	sb := &sandbox.Sandbox{Runtime: &sandbox.Fake{}, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir(), Logf: func(string, ...any) {}}
	fake := sb.Runtime.(*sandbox.Fake)
	h := &harness.ClaudeCode{Sandbox: sb}
	r, item, logs := toolRunner(t, f, h)
	r.Sandbox = sb
	r.MCP = MeshContextMCP("mesh", gatewayMCP.URL)

	tf := &TokenFiles{Token: func(context.Context) (string, error) { return proxyMachineBearer, nil }}
	unbind := tf.Bind(item.ID, item.Attempts, func(context.Context) (string, error) { return proxySessionBearer, nil })
	defer unbind()
	root := shortProxyRoot(t)
	r.Proxies = &SessionProxies{Bearer: tf.Source, Root: root, Logf: t.Logf}

	forbidden := []string{searchSecret, fsSecret, proxySessionBearer, proxyMachineBearer, "mesh-session"}
	type seen struct {
		env     []string
		mounts  []sandbox.Mount
		config  string
		files   map[string]string
		replies map[string]string
		errs    map[string]error
	}
	var got seen
	fake.Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if spec.Stdin != nil {
			_, _ = io.Copy(io.Discard, spec.Stdin)
		}
		got = seen{env: spec.Env, mounts: spec.Mounts, files: map[string]string{}, replies: map[string]string{}, errs: map[string]error{}}
		for i, a := range spec.Command {
			if a == "--mcp-config" && i+1 < len(spec.Command) {
				b, err := os.ReadFile(spec.Command[i+1])
				if err != nil {
					t.Errorf("--mcp-config %s: %v", spec.Command[i+1], err)
				}
				got.config = string(b)
			}
		}
		for _, m := range spec.Mounts {
			_ = filepath.WalkDir(m.Source, func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return nil
				}
				b, _ := os.ReadFile(p)
				got.files[p] = string(b)
				return nil
			})
		}
		// the agent's MCP client, through each shim's socket
		var cfg struct {
			MCPServers map[string]struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(got.config), &cfg); err != nil {
			t.Errorf("mcp config: %v\n%s", err, got.config)
		}
		for name, srv := range cfg.MCPServers {
			if name == "mesh-node" {
				continue
			}
			if srv.Command != "node" || len(srv.Args) != 3 || srv.Args[2] != name {
				t.Errorf("%s is not the proxy's shim: %+v", name, srv)
				continue
			}
			got.replies[name], got.errs[name] = callThrough(srv.Args[1], name)
		}
		fmt.Fprintln(spec.Stdout, `{"type":"system","subtype":"init","session_id":"s-1"}`)
		fmt.Fprintln(spec.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s-1","total_cost_usd":0.5}`)
		return sandbox.Exit{}, nil
	}

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if got.config == "" {
		t.Fatalf("the agent never ran: %v\n%s", fake.Calls, logs.String())
	}

	if !strings.Contains(strings.Join(mapKeys(got.files), "\n"), "/mcp.json") {
		t.Fatalf("the walk read %d mounted files: %v", len(got.files), got.mounts)
	}
	// nothing of a credential in the pod
	for _, s := range forbidden {
		for _, kv := range got.env {
			if strings.Contains(kv, s) {
				t.Errorf("pod env %q holds %q", kv, s)
			}
		}
		if strings.Contains(got.config, s) {
			t.Errorf("--mcp-config holds %q:\n%s", s, got.config)
		}
		for p, body := range got.files {
			if strings.Contains(body, s) || strings.Contains(p, s) {
				t.Errorf("mounted file %s holds %q", p, s)
			}
		}
	}
	for _, kv := range got.env {
		if strings.HasPrefix(kv, sandbox.ToolEnvPrefix) {
			t.Errorf("pod env has a tool variable: %s", kv)
		}
	}
	for p := range got.files {
		if filepath.Base(p) == harness.TokenFileName {
			t.Errorf("a token file is mounted: %s", p)
		}
	}
	proxyMounted := false
	for _, m := range got.mounts {
		if underDir(m.Source, root) {
			proxyMounted = true
		}
	}
	if !proxyMounted {
		t.Errorf("no mount under the MCP root %s: %+v", root, got.mounts)
	}

	// every server reached through the socket, on its own credential
	for name, want := range map[string]string{tools.Builtin: "runsheet answered", "search": "search answered", "fs-tools": "fs key ok"} {
		if err := got.errs[name]; err != nil {
			t.Errorf("%s through the socket: %v", name, err)
			continue
		}
		if !strings.Contains(got.replies[name], want) {
			t.Errorf("%s answered %q, want %q", name, got.replies[name], want)
		}
	}
	if !search.sawAuth("Bearer " + searchSecret) {
		t.Errorf("search upstream never saw its bearer: %v", search.auths)
	}
	if !gatewayMCP.sawAuth("Bearer " + proxySessionBearer) {
		t.Errorf("runsheet upstream never saw the session bearer: %v", gatewayMCP.auths)
	}
	if gatewayMCP.sawAuth("Bearer " + proxyMachineBearer) {
		t.Error("runsheet upstream was sent the machine bearer")
	}

	// the proxy closes with the session
	if left, _ := os.ReadDir(filepath.Join(root, "px")); len(left) != 0 {
		t.Errorf("proxy dirs left after the session: %v", left)
	}
	for _, s := range forbidden[:3] {
		if strings.Contains(logs.String(), s) {
			t.Errorf("node log holds %q", s)
		}
	}
}

// A runsheet upstream that fails: the tools note names the proxy's own
// error, not the agent's bare "failed".
func TestProxyErrorNamedInTheToolsNote(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	px := &SessionProxies{Root: shortProxyRoot(t), Bearer: func(harness.Kind, harness.SessionMeta) (TokenSource, error) {
		return func(context.Context) (string, error) { return proxySessionBearer, nil }, nil
	}}
	job := harness.Job{Kind: harness.KindBuild}
	acct := &SessionAccount{}
	closeProxy, err := SessionTools{}.UseVia(px, "", &job, MeshContextMCP("mesh", down.URL), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy()
	socket, name, ok := harness.ProxiedOf(job.MCPServers[tools.Builtin])
	if !ok || name != tools.Builtin {
		t.Fatalf("runsheet is not proxied: %v", job.MCPServers)
	}
	if _, err := callThrough(socket, name); err == nil {
		t.Fatal("a failing upstream answered")
	}
	res := harness.Result{Tools: map[string]*harness.ToolStat{tools.Builtin: {Errors: 1, FirstError: "failed"}}}
	note := acct.UseNote(res)
	want := "tools: runsheet served, never called: sessionproxy: runsheet: POST " + down.URL + "/mcp answered 503"
	if note != want {
		t.Fatalf("note = %q\nwant  %q", note, want)
	}
	if strings.Contains(note, proxySessionBearer) {
		t.Fatalf("note holds the bearer: %q", note)
	}
}

// The proxy's socket dir is the only thing of it a pod mounts, and a
// proxied session's pod env admits no tool variable.
func TestProxyDirMountedAndToolEnvDropped(t *testing.T) {
	sb := &sandbox.Sandbox{Runtime: &sandbox.Fake{}, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir(), Logf: func(string, ...any) {}}
	sess, err := sb.Open("x", "build", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	env := []string{"MESH_TOOL_SEARCH_AUTHORIZATION=" + searchSecret, "GH_TOKEN=ghs_x"}
	if e := strings.Join(sess.Env(env), "\n"); !strings.Contains(e, searchSecret) {
		t.Fatalf("an unproxied session's tool variable is dropped: %s", e)
	}
	dir := t.TempDir()
	sess.ShareProxyDir(dir)
	if e := strings.Join(sess.Env(env), "\n"); strings.Contains(e, "MESH_TOOL_") || !strings.Contains(e, "GH_TOKEN=ghs_x") {
		t.Fatalf("proxied env = %s", e)
	}
	ws := t.TempDir()
	mounts, err := sess.CheckedMounts(ws)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mounts {
		if m.Source == dir && m.Target == dir && !m.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Fatalf("socket dir not mounted: %+v", mounts)
	}
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
