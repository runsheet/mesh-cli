//go:build !windows

package node

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
	"projectmesh.io/mesh-cli/internal/tools"
)

// proxiedAdoptNode is adoptNode with the session proxy wired as node
// start wires it: the built-in relayed to gatewayMCP, the item's tools
// read at claim time, each build on its own mesh session's bearer.
func proxiedAdoptNode(t *testing.T, f *gatewaytest.Fake, cfg *Config, root, mcpRoot, gatewayMCP string, h harness.Harness) (*Loop, *Runner) {
	t.Helper()
	loop, r := adoptNode(t, f, cfg, root, h)
	gw := gateway.New(f.URL(), "rs_test")
	tf := &TokenFiles{Token: gw.Bearer}
	r.Bearers = &SessionBearers{GW: gw, Tokens: tf, Audience: MeshMCPURL(f.URL())}
	r.Proxies = &SessionProxies{Bearer: tf.Source, Root: mcpRoot, Logf: t.Logf}
	r.MCP = MeshContextMCP("mesh", gatewayMCP)
	r.Runtimes = onlyNpx
	r.Declines = &Declines{}
	return loop, r
}

// A host build reaching runsheet, a remote and a local tool through its
// session proxy keeps running when its node stops; the proxy stops with
// that node, its directory kept. A second node adopts the session and
// restarts a proxy on the same socket path: a tool call through it
// reaches each upstream on its own credential — runsheet on the session's
// re-opened mesh session bearer — and state.json names the socket and
// servers and holds no secret or bearer.
func TestProxyAdoptedSessionAnswersOnTheSameSocket(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	t.Setenv("SLOW_EVENTS", "35")
	t.Setenv("SLOW_SLEEP", "0.1")
	fakeNpx(t)
	search := newMCPUpstream(t, "search answered")
	gatewayMCP := newMCPUpstream(t, "runsheet answered")

	f := seed(t)
	issueClaude(f, item1)
	rt := remoteTool()
	rt["url"] = search.URL + "/mcp"
	f.SessionTools[item1] = []map[string]any{rt, localTool()}
	cfg := registeredNode(t, f)
	root := t.TempDir()
	mcpRoot := shortProxyRoot(t)
	claude := &countedClaude{ClaudeCode: &harness.ClaudeCode{Binary: slowClaudeScript(t), Unit: sessionunit.Detached{}}}

	// node 1 claims the item and starts its session behind a proxy
	loop1, _ := proxiedAdoptNode(t, f, cfg, root, mcpRoot, gatewayMCP.URL, claude)
	ctx1, stop1 := context.WithCancel(context.Background())
	ran1 := make(chan error, 1)
	go func() { ran1 <- loop1.Run(ctx1) }()
	var found []FoundSession
	waitFor(t, func() bool {
		found, _ = FindSessions(root, []string{cfg.NodeID})
		return len(found) == 1 && found[0].State.Offset > 0
	})
	ps := found[0].State.Proxy
	if ps == nil || ps.Socket == "" {
		t.Fatalf("state.json names no proxy: %+v", found[0].State)
	}
	if want := []string{"fs-tools", tools.Builtin, "search"}; !slices.Equal(ps.Servers, want) {
		t.Fatalf("state's proxy servers = %v, want %v", ps.Servers, want)
	}
	if !underDir(ps.Socket, mcpRoot) {
		t.Fatalf("socket %s is not under the MCP root %s", ps.Socket, mcpRoot)
	}
	if out, err := callThrough(ps.Socket, "search"); err != nil || !strings.Contains(out, "search answered") {
		t.Fatalf("node 1's proxy: %q, %v", out, err)
	}
	stop1()
	select {
	case <-ran1:
	case <-time.After(5 * time.Second):
		t.Fatal("loop 1's Run did not return")
	}
	pid := found[0].State.PID
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("the session (pid %d) died with its node", pid)
	}
	// the proxy went with node 1; the directory the agent dials into stays
	if _, err := callThrough(ps.Socket, "search"); err == nil {
		t.Fatal("node 1's proxy still answers after its node stopped")
	}
	if fi, err := os.Stat(filepath.Dir(ps.Socket)); err != nil || !fi.IsDir() {
		t.Fatalf("the proxy's directory went with node 1: %v", err)
	}
	// a socket file left as a killed node leaves it: removed before listening
	staleSocket(t, ps.Socket)

	// node 2 adopts the session and restarts the proxy on the same path
	loop2, _ := proxiedAdoptNode(t, f, cfg, root, mcpRoot, gatewayMCP.URL, claude)
	found, err := FindSessions(root, []string{cfg.NodeID})
	if err != nil || len(found) != 1 || !found[0].Alive() {
		t.Fatalf("found %+v, %v", found, err)
	}
	ctx2, stop2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop2()
	if n := loop2.AdoptSessions(ctx2, found); n != 1 {
		t.Fatalf("adopted %d", n)
	}
	replies := map[string]string{}
	waitFor(t, func() bool {
		out, err := callThrough(ps.Socket, "search")
		replies["search"] = out
		return err == nil
	})
	for _, name := range []string{tools.Builtin, "fs-tools"} {
		out, err := callThrough(ps.Socket, name)
		if err != nil {
			t.Fatalf("%s through the adopted proxy: %v", name, err)
		}
		replies[name] = out
	}
	for name, want := range map[string]string{tools.Builtin: "runsheet answered", "search": "search answered", "fs-tools": "fs key ok"} {
		if !strings.Contains(replies[name], want) {
			t.Errorf("%s answered %q, want %q", name, replies[name], want)
		}
	}
	if !search.sawAuth("Bearer " + searchSecret) {
		t.Errorf("search upstream never saw its bearer: %v", search.auths)
	}
	if n := len(f.CallsNamed("openMeshSession")); n != 2 {
		t.Errorf("mesh session opened %d times, want once per node", n)
	}
	ms := f.MeshSessionFor(item1)
	if ms == nil || len(ms.Minted) == 0 {
		t.Fatalf("no id token minted: %+v", ms)
	}
	if last := ms.Minted[len(ms.Minted)-1]; !gatewayMCP.sawAuth("Bearer " + last) {
		t.Errorf("runsheet upstream never saw the adopted session's bearer %s: %v", last, gatewayMCP.auths)
	}
	if gatewayMCP.sawAuth("Bearer rs_test") {
		t.Error("runsheet upstream was sent the machine bearer")
	}

	// state.json: the socket and the server names, never a secret
	b, err := os.ReadFile(filepath.Join(found[0].Dir, sessionunit.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	state := string(b)
	if !strings.Contains(state, `"socket": "`+ps.Socket+`"`) {
		t.Errorf("state.json does not name the socket:\n%s", state)
	}
	forbidden := append([]string{searchSecret, fsSecret, "rs_test", "Bearer", "mst_", "rsr_", ms.RequestToken}, ms.Minted...)
	for _, s := range forbidden {
		if s != "" && strings.Contains(state, s) {
			t.Errorf("state.json holds %q:\n%s", s, state)
		}
	}

	waitFor(t, func() bool { return f.Item(item1)["status"] == "evaluating" })
	loop2.wg.Wait()
	if n := claude.starts.Load(); n != 1 {
		t.Errorf("claude started %d times", n)
	}
	// the adopted session's end closes its proxy and its directory
	if _, err := os.Stat(filepath.Dir(ps.Socket)); !os.IsNotExist(err) {
		t.Errorf("the proxy's directory outlived the adopted session: %v", err)
	}
}

// staleSocket leaves a unix socket at path with nothing listening, as a
// node killed before its proxy's Close leaves one.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("no stale socket at %s: %v", path, err)
	}
}
