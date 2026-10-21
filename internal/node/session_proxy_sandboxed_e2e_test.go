//go:build e2e

package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
)

// proxyE2EResults, proxyE2EDone: the stub agent writes what each MCP
// server answered it to the first in its workspace, then waits for the
// second — the test's word that its greps inside the agent's container
// are done — before it ends, so the container is still running for
// `podman exec`.
const (
	proxyE2EResults = ".mesh-e2e-proxy.json"
	proxyE2EDone    = ".mesh-e2e-proxy-done"
)

// proxyStubAgent stands in for claude in the pod: a Node script that
// reads its prompt, reads the --mcp-config the harness passes it and,
// for every server but the node's own mesh-node, starts the entry's
// command (the pod shim, `node <shim> <socket> <name>`) and speaks MCP
// on its stdio as Claude Code would — initialize, initialized, one
// tools/call. It writes each server's answer to proxyE2EResults, waits
// for proxyE2EDone, and ends as claude's stream-json does: an init
// event naming each server connected or failed, a tool_use and its
// tool_result per call, the result. Nothing in it names a secret or the
// string the in-container grep looks for.
func proxyStubAgent() string {
	return `#!/usr/bin/env node
"use strict";
const fs = require("fs");
const cp = require("child_process");
const path = require("path");

const tools = { runsheet: "mesh_search", search: "look", "fs-tools": "read" };

function call(name, srv) {
  return new Promise((resolve) => {
    const child = cp.spawn(srv.command, srv.args || [], { stdio: ["pipe", "pipe", "pipe"] });
    let out = "", err = "";
    const timer = setTimeout(() => child.kill("SIGKILL"), 60000);
    child.stdout.on("data", (c) => (out += c));
    child.stderr.on("data", (c) => (err += c));
    child.on("error", (e) => (err += String(e)));
    child.on("close", () => {
      clearTimeout(timer);
      const replies = out.split("\n").filter((l) => l.trim()).map((l) => { try { return JSON.parse(l); } catch (e) { return null; } }).filter(Boolean);
      const reply = replies.find((m) => m.id === 2);
      const ok = !!reply && !reply.error && !!reply.result;
      const text = ok ? JSON.stringify(reply.result) : (reply && reply.error ? reply.error.message : "no reply: " + err.slice(0, 300));
      resolve({ name, ok, text, command: srv.command, args: srv.args || [] });
    });
    const msgs = [
      { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "e2e-agent", version: "1" } } },
      { jsonrpc: "2.0", method: "notifications/initialized" },
      { jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: tools[name] || "look", arguments: {} } },
    ];
    child.stdin.end(msgs.map((m) => JSON.stringify(m)).join("\n") + "\n");
  });
}

async function main() {
  try { fs.readFileSync(0); } catch (e) {}
  const argv = process.argv.slice(2);
  const at = argv.indexOf("--mcp-config");
  const cfg = JSON.parse(fs.readFileSync(argv[at + 1], "utf8"));
  const names = Object.keys(cfg.mcpServers || {}).filter((n) => n !== "mesh-node").sort();
  const results = [];
  for (const n of names) results.push(await call(n, cfg.mcpServers[n]));
  fs.writeFileSync(path.join(process.cwd(), "` + proxyE2EResults + `"), JSON.stringify(results));
  const done = path.join(process.cwd(), "` + proxyE2EDone + `");
  const until = Date.now() + 15 * 60 * 1000;
  while (!fs.existsSync(done) && Date.now() < until) await new Promise((r) => setTimeout(r, 250));

  const say = (m) => process.stdout.write(JSON.stringify(m) + "\n");
  say({ type: "system", subtype: "init", session_id: "e2e", mcp_servers: results.map((r) => ({ name: r.name, status: r.ok ? "connected" : "failed" })) });
  results.forEach((r, i) => {
    const id = "toolu_e2e_" + i;
    say({ type: "assistant", session_id: "e2e", message: { id: "msg_e2e_" + i, content: [{ type: "tool_use", id, name: "mcp__" + r.name + "__" + (tools[r.name] || "look"), input: {} }] } });
    say({ type: "user", session_id: "e2e", message: { content: [{ type: "tool_result", tool_use_id: id, is_error: !r.ok, content: r.text }] } });
  });
  say({ type: "result", subtype: "success", is_error: false, session_id: "e2e", total_cost_usd: 0, result: results.map((r) => r.name + (r.ok ? " ok" : " FAILED")).join(", ") });
}
main();
`
}

// proxyE2EResult is one server's line of proxyE2EResults.
type proxyE2EResult struct {
	Name    string   `json:"name"`
	OK      bool     `json:"ok"`
	Text    string   `json:"text"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// TestSessionProxySandboxedE2E proves the session proxy (mesh-cli
// ADR-038) in a real pod from the node image: Runner.RunItem runs a
// build whose item has a remote tool with a secret header and a local
// tool with a secret env beside the built-in runsheet, the node's
// SessionProxies on, the agent a stub that calls each of the three
// through the pod shim. Fake upstreams on the host record the
// credential each was sent — the search tool its header, the runsheet
// upstream the session's own bearer, the local tool (run by the proxy
// on the host, `npx` faked on PATH) its env. While the agent's container
// still runs, `podman exec` greps its whole filesystem for
// `mesh-session` and for each secret and finds nothing; the build's
// notes carry no `runsheet served, never called`; and no pod or proxy
// directory is left after. It needs podman and the node image, so it
// runs only with MESH_SANDBOX_E2E=1 (scripts/e2e-sandbox.sh).
func TestSessionProxySandboxedE2E(t *testing.T) {
	if os.Getenv("MESH_SANDBOX_E2E") != "1" {
		t.Skip("MESH_SANDBOX_E2E=1 runs real pods")
	}
	image := os.Getenv("MESH_SANDBOX_IMAGE")
	if image == "" {
		image = sandbox.DefaultImage("dev")
	}
	rt := &sandbox.Podman{}
	if _, err := rt.Version(context.Background()); err != nil {
		t.Fatalf("podman: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	fakeNpx(t)
	search := newMCPUpstream(t, "search answered")
	gatewayMCP := newMCPUpstream(t, "runsheet answered")

	slug := fmt.Sprintf("e2e-proxy-%d", time.Now().UnixNano()%1e6)
	f := seed(t)
	f.Goals[goalID]["slug"] = slug
	remote := remoteTool()
	remote["url"] = search.URL + "/mcp"
	f.SessionTools[item1] = []map[string]any{remote, localTool()}

	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{Image: image}.Settings("dev"), TempDir: t.TempDir()}
	h := &harness.ClaudeCode{Sandbox: sb, Stderr: os.Stderr}
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	logs := &syncBuf{}
	r.Log = log.New(io.MultiWriter(logs, os.Stderr), "", 0)
	r.Credentials = &Credentials{GW: r.GW.(CredentialGateway), Log: r.Log}
	issueClaude(f, item.ID)
	r.Runtimes = onlyNpx
	r.Sandbox, r.Declines = sb, &Declines{}
	r.MCP = MeshContextMCP("mesh", gatewayMCP.URL)

	tf := &TokenFiles{Token: func(context.Context) (string, error) { return proxyMachineBearer, nil }}
	unbind := tf.Bind(item.ID, item.Attempts, func(context.Context) (string, error) { return proxySessionBearer, nil }, nil)
	defer unbind()
	root := shortProxyRoot(t)
	r.Proxies = &SessionProxies{Bearer: tf.Source, Root: root, Logf: t.Logf}

	dir := filepath.Join(ws.root, slug, "1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.SandboxBinary = filepath.Join(dir, ".mesh-e2e-agent")
	if err := os.WriteFile(h.SandboxBinary, []byte(proxyStubAgent()), 0o755); err != nil {
		t.Fatal(err)
	}

	secrets := map[string]string{
		"the search tool's header": searchSecret,
		"the local tool's env":     fsSecret,
		"the session's bearer":     proxySessionBearer,
		"the machine's bearer":     proxyMachineBearer,
	}

	// while the agent waits: grep its container
	type greps struct {
		agent string
		found map[string]string // what was grepped for → the files that hold it
		err   error
	}
	grepped := make(chan greps, 1)
	go func() {
		g := greps{found: map[string]string{}}
		defer func() {
			// the agent ends either way
			_ = os.WriteFile(filepath.Join(dir, proxyE2EDone), nil, 0o644)
			grepped <- g
		}()
		if g.err = waitForFile(ctx, filepath.Join(dir, proxyE2EResults)); g.err != nil {
			return
		}
		if g.agent, g.err = agentContainer(ctx, "mesh-"+slug+"-"); g.err != nil {
			return
		}
		patterns := []string{"mesh-session"}
		for _, v := range secrets {
			patterns = append(patterns, v)
		}
		for _, p := range patterns {
			out, err := grepInContainer(ctx, g.agent, p)
			if err != nil {
				g.err = err
				return
			}
			g.found[p] = out
		}
	}()

	runErr := r.RunItem(ctx, item)
	g := <-grepped
	if runErr != nil {
		t.Fatalf("build: %v\n%s", runErr, logs.String())
	}
	if g.err != nil {
		t.Fatalf("in-container grep: %v", g.err)
	}

	// the agent reached every server through its socket
	b, err := os.ReadFile(filepath.Join(dir, proxyE2EResults))
	if err != nil {
		t.Fatalf("the agent did not run: %v", err)
	}
	var results []proxyE2EResult
	if err := json.Unmarshal(b, &results); err != nil {
		t.Fatalf("%s: %v\n%s", proxyE2EResults, err, b)
	}
	answered := map[string]proxyE2EResult{}
	for _, res := range results {
		answered[res.Name] = res
		if res.Command != "node" || len(res.Args) != 3 || filepath.Base(res.Args[0]) != harness.ProxyShimScriptName || res.Args[2] != res.Name {
			t.Errorf("%s is not the pod shim: %s %v", res.Name, res.Command, res.Args)
		}
	}
	for name, want := range map[string]string{tools.Builtin: "runsheet answered", "search": "search answered", "fs-tools": "fs key ok"} {
		res, ok := answered[name]
		switch {
		case !ok:
			t.Errorf("%s: not in the agent's MCP config: %s", name, b)
		case !res.OK || !strings.Contains(res.Text, want):
			t.Errorf("%s through the pod shim: ok=%v %q, want %q", name, res.OK, res.Text, want)
		default:
			t.Logf("agent: %s through the pod shim answered %q", name, want)
		}
	}

	// each fake upstream on the host got its credential from the proxy
	if search.sawAuth("Bearer " + searchSecret) {
		t.Logf("upstream search received its credential: Authorization: Bearer <the search tool's header>")
	} else {
		t.Errorf("search upstream never saw its bearer: %v", search.auths)
	}
	if gatewayMCP.sawAuth("Bearer " + proxySessionBearer) {
		t.Logf("upstream runsheet received its credential: Authorization: Bearer <the session's bearer>")
	} else {
		t.Errorf("runsheet upstream never saw the session's bearer: %v", gatewayMCP.auths)
	}
	if gatewayMCP.sawAuth("Bearer " + proxyMachineBearer) {
		t.Error("runsheet upstream was sent the machine's bearer")
	}
	if res := answered["fs-tools"]; res.OK && strings.Contains(res.Text, "fs key ok") {
		t.Logf("upstream fs-tools received its credential: FS_API_KEY in the env of the server the proxy ran on the host")
	}

	// nothing of a credential in the agent's container
	t.Logf("podman exec %s sh -c 'grep -rlF -f - / 2>/dev/null' <<< mesh-session: %s", g.agent, foundOrNothing(g.found["mesh-session"]))
	if out := g.found["mesh-session"]; out != "" {
		t.Errorf("the agent's container holds mesh-session in:\n%s", out)
	}
	for what, v := range secrets {
		t.Logf("podman exec %s sh -c 'grep -rlF -f - / 2>/dev/null' <<< <%s>: %s", g.agent, what, foundOrNothing(g.found[v]))
		if out := g.found[v]; out != "" {
			t.Errorf("the agent's container holds %s in:\n%s", what, out)
		}
	}

	// the notes name no runsheet failure
	var notes []string
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if n, ok := c.Input["note"].(string); ok {
			notes = append(notes, n)
		}
	}
	all := strings.Join(notes, "\n") + "\n" + logs.String()
	if strings.Contains(all, "runsheet served, never called") {
		t.Errorf("the session's notes say runsheet was never called:\n%s", all)
	} else {
		t.Logf("notes: no `runsheet served, never called` in %d note(s)", len(notes))
	}
	for what, v := range secrets {
		if strings.Contains(all, v) {
			t.Errorf("notes or the node's log hold %s", what)
		}
	}

	// the session left nothing
	if rel := f.CallsNamed("releaseWorkItem"); len(rel) != 0 {
		t.Errorf("build released: %+v", rel)
	}
	assertNoPods(t, "mesh-"+slug+"-")
	if left, _ := os.ReadDir(filepath.Join(root, "px")); len(left) != 0 {
		t.Errorf("proxy dirs left after the session: %v", left)
	}
	if left, _ := os.ReadDir(sb.TempDir); len(left) != 0 {
		t.Errorf("session HOMEs left: %v", left)
	}
}

// waitForFile waits for path to exist.
func waitForFile(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", path, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// agentContainer is the running agent container whose name starts with
// prefix (sandbox.AgentName of the session's pod).
func agentContainer(ctx context.Context, prefix string) (string, error) {
	out, err := exec.CommandContext(ctx, "podman", "ps", "--format", "{{.Names}}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("podman ps: %v: %s", err, out)
	}
	for _, name := range strings.Fields(string(out)) {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, sandbox.AgentName(sandbox.Pod{})) {
			return name, nil
		}
	}
	return "", fmt.Errorf("no running agent container named %s…-agent: %s", prefix, out)
}

// grepInContainer is `podman exec <agent> sh -c 'grep -rl <pattern> /
// 2>/dev/null'`, the files under / that hold pattern — the pattern
// given on stdin (-f -), never on the command line: grep's own
// /proc/<pid>/cmdline would otherwise hold it and match.
func grepInContainer(ctx context.Context, agent, pattern string) (string, error) {
	cmd := exec.CommandContext(ctx, "podman", "exec", "-i", agent, "sh", "-c", "grep -rlF -f - / 2>/dev/null")
	cmd.Stdin = strings.NewReader(pattern + "\n")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	// grep exits 1 for no match, 2 for an unreadable file among matches
	if ee := (*exec.ExitError)(nil); err != nil && !(errors.As(err, &ee) && ee.ExitCode() <= 2) {
		return "", fmt.Errorf("podman exec %s grep: %v: %s", agent, err, errb.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func foundOrNothing(out string) string {
	if out == "" {
		return "found nothing"
	}
	return "FOUND\n" + out
}
