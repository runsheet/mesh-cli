package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

const relayURL = "https://api.test/mcp"

// bearerPrefix opens every bearer the test keeper writes: it must be in
// no config, command line or state file.
const bearerPrefix = "rs_secret_bearer_"

// rotatingKeeper is a TokenKeeper whose bearer rotates every tick while
// a file is kept: bearerPrefix0, bearerPrefix1, …
type rotatingKeeper struct {
	tick time.Duration

	mu       sync.Mutex
	n        int
	kept     []string
	released map[string]bool // path → remove
}

func (k *rotatingKeeper) Keep(path string) (func(bool), error) {
	k.mu.Lock()
	k.kept = append(k.kept, path)
	tok := fmt.Sprintf("%s%d", bearerPrefix, k.n)
	k.mu.Unlock()
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return nil, err
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		if k.tick == 0 {
			<-stop
			return
		}
		t := time.NewTicker(k.tick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			k.mu.Lock()
			k.n++
			tok := fmt.Sprintf("%s%d", bearerPrefix, k.n)
			k.mu.Unlock()
			tmp := path + ".tmp"
			if os.WriteFile(tmp, []byte(tok+"\n"), 0o600) == nil {
				os.Rename(tmp, path)
			}
		}
	}()
	var once sync.Once
	return func(remove bool) {
		once.Do(func() {
			close(stop)
			<-done
			k.mu.Lock()
			if k.released == nil {
				k.released = map[string]bool{}
			}
			k.released[path] = remove
			k.mu.Unlock()
			if remove {
				os.Remove(path)
			}
		})
	}, nil
}

func (k *rotatingKeeper) keptPaths() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.kept...)
}

func (k *rotatingKeeper) releasedAs(path string) (remove, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	remove, ok = k.released[path]
	return
}

func relayServers() map[string]any {
	return map[string]any{BuiltinMCP: MeshContextRelay("/usr/local/bin/mesh", relayURL, "")}
}

// noBearer fails when any bearer the keeper wrote is in s.
func noBearer(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, bearerPrefix) || strings.Contains(strings.ToLower(s), "authorization") {
		t.Fatalf("%s carries a bearer: %s", what, s)
	}
}

// A host Claude session's mesh-context is the stdio relay on the token
// file in its own session dir, which the keeper writes before the
// session starts and removes once it ends; the command line and
// state.json name the file, never the bearer.
func TestClaudeHostMeshContextIsTheRelayOnASessionTokenFile(t *testing.T) {
	ws := t.TempDir()
	dir := sessionunit.Dir(ws)
	want := filepath.Join(dir, TokenFileName)
	k := &rotatingKeeper{}
	h := &ClaudeCode{Binary: slowClaude(t), Unit: sessionunit.Detached{}, Tokens: k}
	var during sessionunit.State
	var tok []byte
	var mode os.FileMode
	_, err := h.Run(context.Background(), Job{Kind: KindEvaluate, Workspace: ws, MCPServers: relayServers(), Env: []string{"SLOW_EVENTS=2", "SLOW_SLEEP=0.05"}}, func(p Progress) {
		if p.Text == "step 1" {
			during, _ = sessionunit.ReadState(dir)
			tok, _ = os.ReadFile(want)
			if fi, err := os.Stat(want); err == nil {
				mode = fi.Mode().Perm()
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := k.keptPaths(); !slices.Equal(got, []string{want}) {
		t.Fatalf("kept %v, want %s", got, want)
	}
	if !strings.HasPrefix(string(tok), bearerPrefix) || mode != 0o600 {
		t.Fatalf("token file during the session = %q (%v)", tok, mode)
	}
	if during.TokenFile != want {
		t.Fatalf("state.json tokenFile = %q, want %q", during.TokenFile, want)
	}
	if remove, ok := k.releasedAs(want); !ok || !remove {
		t.Fatalf("released %v (remove %v), want removed at the end", ok, remove)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("token file left after the session: %v", err)
	}

	// the command line: a stdio relay naming the file, no bearer
	job, path, release, err := placeMeshContext(Job{Kind: KindBuild, MCPServers: relayServers()}, k, t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer release(true)
	args := strings.Join(h.Args(job), " ")
	noBearer(t, "the claude command line", args)
	srv := job.MCPServers[BuiltinMCP].(map[string]any)
	if srv["type"] != "stdio" || srv["command"] != "/usr/local/bin/mesh" ||
		!slices.Equal(srv["args"].([]string), []string{RelayCommand, "--url", relayURL, "--token-file", path}) {
		t.Fatalf("host mesh-context = %v", srv)
	}
	if !strings.Contains(args, `"mesh-context":{"args":["mcp-relay","--url","`+relayURL+`","--token-file","`+path+`"]`) {
		t.Fatalf("--mcp-config does not name the relay: %s", args)
	}
	if !strings.Contains(args, "mcp__mesh-context__*") {
		t.Fatalf("mesh-context's tools not allowed: %s", args)
	}
}

// A pod's mesh-context is RelayScript in its HOME — the pod has no mesh
// binary — on the token file in that HOME, the mcp.json naming the file
// and never the bearer; the file goes when the session ends.
func TestClaudePodMeshContextIsTheRelayScriptOnAHomeTokenFile(t *testing.T) {
	f, sb := detachedFake(t)
	k := &rotatingKeeper{}
	var cfg, script, tok []byte
	var command []string
	f.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		command = spec.Command
		for i, a := range spec.Command {
			if a == "--mcp-config" && i+1 < len(spec.Command) {
				home := filepath.Dir(spec.Command[i+1])
				cfg, _ = os.ReadFile(spec.Command[i+1])
				script, _ = os.ReadFile(filepath.Join(home, relayScriptName))
				tok, _ = os.ReadFile(filepath.Join(home, TokenFileName))
			}
		}
		spec.Stdout.Write([]byte(evInit + "\n" + evResult + "\n"))
		return sandbox.Exit{}, nil
	}
	ws := t.TempDir()
	res, err := (&ClaudeCode{Sandbox: sb, Tokens: k}).Run(context.Background(), Job{Kind: KindBuild, Label: "g/1", Workspace: ws, Prompt: "p", MCPServers: relayServers()}, nil)
	if err != nil || !res.Success {
		t.Fatalf("run = %+v, %v", res, err)
	}
	noBearer(t, "the pod's command line", strings.Join(command, " "))
	noBearer(t, "the pod's mcp.json", string(cfg))
	var m struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(cfg, &m); err != nil {
		t.Fatalf("mcp.json %s: %v", cfg, err)
	}
	srv := m.MCPServers[BuiltinMCP]
	paths := k.keptPaths()
	if len(paths) != 1 || filepath.Base(paths[0]) != TokenFileName {
		t.Fatalf("kept %v", paths)
	}
	home := filepath.Dir(paths[0])
	if srv.Type != "stdio" || srv.Command != "node" ||
		!slices.Equal(srv.Args, []string{filepath.Join(home, relayScriptName), relayURL, paths[0]}) {
		t.Fatalf("pod mesh-context = %+v", srv)
	}
	if _, ok := m.MCPServers[NodeWorkMCP]; !ok {
		t.Fatalf("a pod build lost its own work tools: %s", cfg)
	}
	if string(script) != string(RelayScript()) || !strings.HasPrefix(string(tok), bearerPrefix) {
		t.Fatalf("relay script %d bytes, token %q", len(script), tok)
	}
	if remove, ok := k.releasedAs(paths[0]); !ok || !remove {
		t.Fatalf("released %v (remove %v)", ok, remove)
	}
}

// A detached session's token file outlives the node that started it —
// released without removal — and the node that adopts it keeps the
// file its state.json names fresh: the bearer in it rotates while the
// adopted session runs, and the file goes when its end is handled.
func TestAdoptedSessionTokenFileIsRefreshed(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	ws := t.TempDir()
	dir := sessionunit.Dir(ws)
	first := &rotatingKeeper{}
	h := &ClaudeCode{Binary: slowClaude(t), Unit: sessionunit.Detached{}, Tokens: first}
	ctx, cancel := context.WithCancel(context.Background())
	var st sessionunit.State
	_, err := h.Run(ctx, Job{Kind: KindBuild, Workspace: ws, Outlive: true, MCPServers: relayServers(),
		Env: []string{"SLOW_EVENTS=30", "SLOW_SLEEP=0.05"}, Meta: SessionMeta{ItemID: "i"}}, func(p Progress) {
		if p.Text == "step 2" {
			st, _ = sessionunit.ReadState(dir)
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	t.Cleanup(func() { syscall.Kill(-st.PID, syscall.SIGKILL) })
	path := filepath.Join(dir, TokenFileName)
	if st.TokenFile != path {
		t.Fatalf("state.json tokenFile = %q, want %q", st.TokenFile, path)
	}
	if remove, ok := first.releasedAs(path); !ok || remove {
		t.Fatalf("the detaching node released %v, remove %v: the file must stay for the adopter", ok, remove)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("token file gone with the node: %v", err)
	}

	adopter := &rotatingKeeper{tick: 20 * time.Millisecond, n: 100}
	h2 := &ClaudeCode{Binary: slowClaude(t), Unit: sessionunit.Detached{}, Tokens: adopter}
	var seen []string
	var mu sync.Mutex
	res, err := h2.Attach(context.Background(), dir, func(p Progress) {
		if b, err := os.ReadFile(path); err == nil {
			mu.Lock()
			seen = append(seen, strings.TrimSpace(string(b)))
			mu.Unlock()
		}
	})
	if err != nil || !res.Success {
		t.Fatalf("attach = %+v, %v", res, err)
	}
	if got := adopter.keptPaths(); !slices.Equal(got, []string{path}) {
		t.Fatalf("adopter kept %v, want the state's %s", got, path)
	}
	mu.Lock()
	defer mu.Unlock()
	rotated := map[string]bool{}
	for _, s := range seen {
		if s != strings.TrimSpace(string(before)) {
			rotated[s] = true
		}
	}
	if len(rotated) < 2 {
		t.Fatalf("the adopted session's token file was not refreshed: before %q, seen %q", before, seen)
	}
	if remove, ok := adopter.releasedAs(path); !ok || !remove {
		t.Fatalf("adopter released %v, remove %v", ok, remove)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("token file left after the adopted session's end: %v", err)
	}
}

// Codex's config.toml renders mesh-context as the stdio relay on a token
// file in the session's CODEX_HOME — command and args, no
// env_http_headers and no bearer — and the file goes with the home.
func TestCodexMeshContextIsTheRelayOnAHomeTokenFile(t *testing.T) {
	home := t.TempDir()
	k := &rotatingKeeper{}
	c := &Codex{Tokens: k}
	h, err := c.prepareHome(Job{MCPServers: relayServers(), Env: []string{"CODEX_HOME=" + home}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, TokenFileName)
	if h.tokenFile != path {
		t.Fatalf("token file %q, want %q", h.tokenFile, path)
	}
	cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	noBearer(t, "config.toml", string(cfg))
	want := `[mcp_servers.mesh-context]
command = "/usr/local/bin/mesh"
args = ["mcp-relay", "--url", "` + relayURL + `", "--token-file", "` + path + `"]
default_tools_approval_mode = "approve"
`
	if string(cfg) != want {
		t.Fatalf("config.toml =\n%s\nwant\n%s", cfg, want)
	}
	for _, kv := range h.env {
		noBearer(t, "the Codex session's env", kv)
	}
	tok, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(tok), bearerPrefix) {
		t.Fatalf("token file %q", tok)
	}
	if err := h.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("token file left after the Codex session: %v", err)
	}

	// CodexMCPConfig on the placed servers: the same stdio table
	servers, _, release, err := PlaceMeshContext(relayServers(), k, t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer release(true)
	toml, env, err := CodexMCPConfig(servers, nil)
	if err != nil || len(env) != 0 || !strings.Contains(toml, `command = "/usr/local/bin/mesh"`) || strings.Contains(toml, "env_http_headers") {
		t.Fatalf("CodexMCPConfig = %s, %v, %v", toml, env, err)
	}
}

// Servers whose mesh-context is no relay — a test's http entry — and a
// harness with no keeper run as they came.
func TestPlaceMeshContextLeavesOtherEntries(t *testing.T) {
	httpSrv := map[string]any{BuiltinMCP: map[string]any{"type": "http", "url": relayURL}}
	got, path, release, err := PlaceMeshContext(httpSrv, &rotatingKeeper{}, t.TempDir(), false, "")
	release(true)
	if err != nil || path != "" || got[BuiltinMCP].(map[string]any)["type"] != "http" {
		t.Fatalf("http entry placed: %v %q %v", got, path, err)
	}
	got, path, release, err = PlaceMeshContext(relayServers(), nil, t.TempDir(), false, "")
	release(true)
	if err != nil || path != "" {
		t.Fatalf("no keeper placed %q, %v", path, err)
	}
	if _, tf, _ := relayOf(got[BuiltinMCP]); tf != "" {
		t.Fatalf("no keeper named token file %q", tf)
	}
}

// sabotageKeeper keeps as rotatingKeeper does and then puts a directory
// where the session's next file goes, so the start fails right after
// the token file is placed.
type sabotageKeeper struct {
	rotatingKeeper
	blocks string
}

func (k *sabotageKeeper) Keep(path string) (func(bool), error) {
	release, err := k.rotatingKeeper.Keep(path)
	if err == nil {
		os.MkdirAll(filepath.Join(filepath.Dir(path), k.blocks), 0o700)
	}
	return release, err
}

// A start that fails after the token file is placed — a pod's mcp.json
// or a supplied CODEX_HOME's config.toml that cannot be written —
// releases the keeper and removes the file: no keeper outlives a
// session that never ran.
func TestTokenKeeperReleasedOnAFailedStart(t *testing.T) {
	_, sb := detachedFake(t)
	k := &sabotageKeeper{blocks: "mcp.json"}
	_, err := (&ClaudeCode{Sandbox: sb, Tokens: k}).Run(context.Background(), Job{Kind: KindBuild, Label: "g/1", Workspace: t.TempDir(), Prompt: "p", MCPServers: relayServers()}, nil)
	if err == nil || !strings.Contains(err.Error(), "mcp config") {
		t.Fatalf("pod run = %v, want the mcp config's failure", err)
	}
	paths := k.keptPaths()
	if len(paths) != 1 {
		t.Fatalf("kept %v", paths)
	}
	if remove, ok := k.releasedAs(paths[0]); !ok || !remove {
		t.Fatalf("pod start failed: released %v (remove %v)", ok, remove)
	}

	home := t.TempDir()
	ck := &sabotageKeeper{blocks: "config.toml"}
	if _, err := (&Codex{Tokens: ck}).prepareHome(Job{MCPServers: relayServers(), Env: []string{"CODEX_HOME=" + home}}); err == nil {
		t.Fatal("prepareHome wrote config.toml over a directory")
	}
	path := filepath.Join(home, TokenFileName)
	if remove, ok := ck.releasedAs(path); !ok || !remove {
		t.Fatalf("codex start failed: released %v (remove %v)", ok, remove)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("token file left after a failed codex start: %v", err)
	}
}
