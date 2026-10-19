package harness

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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// bearerGateway is a fake /mcp that records the Authorization of every
// request and answers each with its own path and that header's bearer.
type bearerGateway struct {
	mu   sync.Mutex
	seen []string
}

func (g *bearerGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	g.mu.Lock()
	g.seen = append(g.seen, r.URL.Path+" "+auth)
	g.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"path": r.URL.Path}})
}

func (g *bearerGateway) last() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.seen) == 0 {
		return ""
	}
	return g.seen[len(g.seen)-1]
}

// callSocket posts one tools/list over the unix socket as the pod's
// relay does, with no Authorization of its own.
func callSocket(t *testing.T, sock string) {
	t.Helper()
	// dialled through its directory's descriptor, as the pod's relay
	// reaches it from its directory: the path may be past sun_path's
	// limit (a long TMPDIR)
	dir, err := os.Open(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	addr := fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(sock))
	c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", addr)
	}}}
	resp, err := c.Post("http://runsheet/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("call through %s: %v", sock, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("call through %s: %d %s", sock, resp.StatusCode, b)
	}
}

// noBearerUnder fails when any regular file under dir holds a bearer the
// keeper wrote, or names the token file.
func noBearerUnder(t *testing.T, what, dir string) {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n++
		if strings.Contains(string(b), bearerPrefix) || strings.Contains(string(b), TokenFileName) {
			t.Errorf("%s: %s holds the bearer or names its file: %.300s", what, p, b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s: walk %s: %v", what, dir, err)
	}
	if n == 0 && what == "the pod's HOME" {
		t.Fatalf("%s: no file under %s", what, dir)
	}
}

// The pod's MCP relay holds no bearer: the rendered mcp.json (Claude) and
// config.toml (Codex) name the node's endpoint socket in the HOME, and no
// file the harness writes under the session's HOME or workspace holds
// the bearer or names its token file — which stays on the node. A call
// through the socket reaches the fake /mcp on the bearer in the
// node-side file of the moment, a rotation included.
func TestRelayBearerStaysOnTheNode(t *testing.T) {
	g := &bearerGateway{}
	srv := httptest.NewServer(g)
	defer srv.Close()
	servers := func() map[string]any {
		return map[string]any{BuiltinMCP: MeshContextRelay("/usr/local/bin/mesh", srv.URL+"/mcp", "")}
	}

	t.Run("claude", func(t *testing.T) {
		f, sb := detachedFake(t)
		k := &rotatingKeeper{}
		ws := t.TempDir()
		// the agent hands its pod's view to the test's own goroutine and
		// waits for the checks there: a Fatal in the agent's goroutine
		// would end it alone and leave Run waiting on it
		type podView struct {
			command, env []string
			config       string
		}
		views := make(chan podView, 1)
		checked := make(chan struct{})
		f.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
			for i, a := range spec.Command {
				if a == "--mcp-config" && i+1 < len(spec.Command) {
					views <- podView{command: spec.Command, env: spec.Env, config: spec.Command[i+1]}
					select {
					case <-checked:
					case <-ctx.Done():
					}
				}
			}
			spec.Stdout.Write([]byte(evInit + "\n" + evResult + "\n"))
			return sandbox.Exit{}, nil
		}
		type outcome struct {
			res Result
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			res, err := (&ClaudeCode{Sandbox: sb, Tokens: k}).Run(context.Background(), Job{Kind: KindBuild, Label: "g/1", Workspace: ws, Prompt: "p", MCPServers: servers()}, nil)
			done <- outcome{res, err}
		}()
		var v podView
		select {
		case v = <-views:
		case o := <-done:
			t.Fatalf("the agent never ran with an mcp config: %+v, %v", o.res, o.err)
		}
		func() {
			defer close(checked)
			noBearer(t, "the pod's command line", strings.Join(v.command, " "))
			for _, kv := range v.env {
				noBearer(t, "the pod's env", kv)
			}
			home := filepath.Dir(v.config)
			noBearerUnder(t, "the pod's HOME", home)
			noBearerUnder(t, "the workspace", ws)
			cfg, err := os.ReadFile(v.config)
			if err != nil {
				t.Fatal(err)
			}
			var m struct {
				MCPServers map[string]struct {
					Args []string `json:"args"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(cfg, &m); err != nil {
				t.Fatal(err)
			}
			args := m.MCPServers[BuiltinMCP].Args
			sock := filepath.Join(home, RelaySocketName)
			if len(args) != 2 || args[1] != sock {
				t.Fatalf("pod runsheet args %v, want the script on %s", args, sock)
			}
			assertCallsCarryTheBearer(t, g, sock, k.keptPaths())
		}()
		if o := <-done; o.err != nil || !o.res.Success {
			t.Fatalf("run = %+v, %v", o.res, o.err)
		}
	})

	t.Run("codex", func(t *testing.T) {
		_, sb := detachedFake(t)
		k := &rotatingKeeper{}
		sess, err := sb.Open("g/7 evaluate", string(KindEvaluate), sandbox.GitIdentity{})
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		placed := t.TempDir()
		if err := os.WriteFile(filepath.Join(placed, "auth.json"), []byte(`{"tokens":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		ws := t.TempDir()
		h, env, _, err := (&Codex{Sandbox: sb, Tokens: k}).prepareSandboxHome(Job{Kind: KindEvaluate, Workspace: ws, MCPServers: servers(), Env: []string{"CODEX_HOME=" + placed}}, sess)
		if err != nil {
			t.Fatal(err)
		}
		for _, kv := range env {
			noBearer(t, "the Codex pod's env", kv)
		}
		noBearerUnder(t, "the pod's HOME", sess.Home)
		noBearerUnder(t, "the workspace", ws)
		cfg, _ := os.ReadFile(filepath.Join(sess.CodexHome(), "config.toml"))
		sock := filepath.Join(sess.Home, RelaySocketName)
		if !strings.Contains(string(cfg), `"`+sock+`"`) {
			t.Fatalf("config.toml does not name the endpoint %s:\n%s", sock, cfg)
		}
		if strings.HasPrefix(h.tokenFile, sess.Home) {
			t.Fatalf("the token file %s is in the pod's HOME", h.tokenFile)
		}
		assertCallsCarryTheBearer(t, g, sock, k.keptPaths())
		if err := h.close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(sock); !os.IsNotExist(err) {
			t.Fatalf("the endpoint socket outlived the session: %v", err)
		}
		if _, err := os.Stat(h.tokenFile); !os.IsNotExist(err) {
			t.Fatalf("the token file outlived the session: %v", err)
		}
	})
}

// assertCallsCarryTheBearer calls through sock twice, the node-side
// token file rotated between, and sees each reach /mcp on the file's
// bearer of the moment.
func assertCallsCarryTheBearer(t *testing.T, g *bearerGateway, sock string, kept []string) {
	t.Helper()
	if len(kept) != 1 {
		t.Fatalf("kept %v, want one node-side token file", kept)
	}
	tok, err := readTokenFile(kept[0])
	if err != nil || !strings.HasPrefix(tok, bearerPrefix) {
		t.Fatalf("token file %s = %q, %v", kept[0], tok, err)
	}
	callSocket(t, sock)
	if got := g.last(); got != "/mcp Bearer "+tok {
		t.Fatalf("the gateway saw %q, want /mcp on the node's bearer", got)
	}
	rotated := bearerPrefix + "rotated"
	if err := os.WriteFile(kept[0], []byte(rotated+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	callSocket(t, sock)
	if got := g.last(); got != "/mcp Bearer "+rotated {
		t.Fatalf("after a rotation the gateway saw %q", got)
	}
}
