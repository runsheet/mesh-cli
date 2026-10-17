//go:build e2e

package node

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/mcprelay"
	"projectmesh.io/mesh-cli/internal/sessionunit"
	"projectmesh.io/mesh-cli/internal/tools"
)

// The node calls the context server runsheet, end to end (goal
// the-node-calls-the-context-server-runsheet; ADR-006's amendment
// 2026-10-15): a build session the Runner starts through the real
// Claude Code harness gets an MCP config whose built-in is `runsheet`,
// that server's relay carries the session's tools/list to the platform's
// /mcp (the gatewaytest fake, which records it), and the session's
// mcp__runsheet__mesh_finish is the finish the node hands in. A session
// an older node started — its token file mesh-context.token, its stream
// naming mesh-context — is adopted and still finishes.
//
// The test binary plays both programs the session runs: named `claude`
// it is the agent (runsheetFakeClaude), named `mesh` it is `mesh
// mcp-relay` (the real mcprelay.Relay). TestMain picks by the name it
// was started under, so the config the node rendered is what runs.

// runsheetE2EDir is where the fake claude leaves what it saw: argv.json
// (its command line) and tools.json (the names tools/list answered).
const runsheetE2EDir = "RUNSHEET_E2E_DIR"

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "claude":
		os.Exit(runsheetFakeClaude())
	case "mesh":
		os.Exit(runsheetFakeRelay())
	}
	os.Exit(m.Run())
}

// runsheetFakeRelay is `mesh mcp-relay --url <u> --token-file <p>`.
func runsheetFakeRelay() int {
	args := os.Args[1:]
	if len(args) == 0 || args[0] != harness.RelayCommand {
		fmt.Fprintf(os.Stderr, "fake mesh: unexpected %q\n", args)
		return 2
	}
	r := &mcprelay.Relay{Stderr: os.Stderr}
	for i := 1; i+1 < len(args); i++ {
		switch args[i] {
		case "--url":
			r.URL = args[i+1]
		case "--token-file":
			r.TokenFile = args[i+1]
		}
	}
	if err := r.Run(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fake mesh:", err)
		return 1
	}
	return 0
}

// runsheetFakeClaude is `claude -p`: it reads its prompt, starts the
// `runsheet` server of its --mcp-config, initializes it and lists its
// tools, then streams a session that calls mcp__runsheet__mesh_finish.
func runsheetFakeClaude() int {
	_, _ = io.Copy(io.Discard, os.Stdin)
	out := os.Getenv(runsheetE2EDir)
	fail := func(format string, a ...any) int {
		msg := fmt.Sprintf(format, a...)
		fmt.Fprintln(os.Stderr, "fake claude:", msg)
		b, _ := json.Marshal(map[string]any{"type": "result", "subtype": "error", "is_error": true, "session_id": "s-runsheet", "result": msg})
		fmt.Println(string(b))
		return 1
	}
	argv, _ := json.Marshal(os.Args[1:])
	if err := os.WriteFile(filepath.Join(out, "argv.json"), argv, 0o600); err != nil {
		return fail("argv: %v", err)
	}
	cfg := flagValue(os.Args[1:], "--mcp-config")
	var mc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg), &mc); err != nil {
		return fail("mcp config: %v", err)
	}
	srv, ok := mc.MCPServers[tools.Builtin]
	if !ok {
		return fail("no %s server in the mcp config", tools.Builtin)
	}
	cmd := exec.Command(srv.Command, srv.Args...)
	cmd.Stderr = os.Stderr
	in, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return fail("start %s: %v", tools.Builtin, err)
	}
	replies := bufio.NewScanner(stdout)
	replies.Buffer(make([]byte, 0, 64*1024), 1<<20)
	call := func(msg string, reply bool) (json.RawMessage, error) {
		if _, err := io.WriteString(in, msg+"\n"); err != nil {
			return nil, err
		}
		if !reply {
			return nil, nil
		}
		if !replies.Scan() {
			return nil, fmt.Errorf("no reply to %s: %v", msg, replies.Err())
		}
		var r struct {
			Result json.RawMessage `json:"result"`
			Error  any             `json:"error"`
		}
		if err := json.Unmarshal(replies.Bytes(), &r); err != nil || r.Error != nil {
			return nil, fmt.Errorf("reply %s: %v %v", replies.Text(), err, r.Error)
		}
		return r.Result, nil
	}
	if _, err := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake-claude","version":"0"}}}`, true); err != nil {
		return fail("initialize: %v", err)
	}
	if _, err := call(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, false); err != nil {
		return fail("initialized: %v", err)
	}
	listed, err := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, true)
	if err != nil {
		return fail("tools/list: %v", err)
	}
	var tl struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(listed, &tl)
	var names []string
	for _, t := range tl.Tools {
		names = append(names, t.Name)
	}
	b, _ := json.Marshal(names)
	if err := os.WriteFile(filepath.Join(out, "tools.json"), b, 0o600); err != nil {
		return fail("tools: %v", err)
	}
	in.Close()
	_ = cmd.Wait()

	for _, line := range runsheetFinishStream("s-runsheet", tools.Builtin, os.Getenv("RUNSHEET_E2E_ITEM")) {
		fmt.Println(line)
	}
	return 0
}

// runsheetFinishStream is a build session's stream-json on server: an
// init naming it connected, a mesh_finish call on it and its answer, and
// the result.
func runsheetFinishStream(session, server, itemID string) []string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return []string{
		enc(map[string]any{"type": "system", "subtype": "init", "session_id": session, "model": "claude-sonnet-5",
			"mcp_servers": []any{map[string]any{"name": server, "status": "connected"}}}),
		enc(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"role": "assistant", "id": "msg_1",
			"content": []any{map[string]any{"type": "tool_use", "id": "toolu_finish", "name": "mcp__" + server + "__mesh_finish",
				"input": map[string]any{"item_id": itemID, "branch": "mesh/ship-it-1", "head_sha": "abc123def456"}}}}}),
		enc(map[string]any{"type": "user", "session_id": session, "message": map[string]any{"role": "user",
			"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_finish", "content": "recorded"}}}}),
		enc(map[string]any{"type": "result", "subtype": "success", "session_id": session, "result": "Finished by tool call.", "total_cost_usd": 0.25, "num_turns": 2}),
	}
}

// runsheetBinaries links the test binary as `claude` and `mesh` in a
// temporary dir, returning both paths.
func runsheetBinaries(t *testing.T) (claude, mesh string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	claude, mesh = filepath.Join(dir, "claude"), filepath.Join(dir, "mesh")
	for _, p := range []string{claude, mesh} {
		if err := os.Symlink(self, p); err != nil {
			t.Fatal(err)
		}
	}
	return claude, mesh
}

// fixedBearer keeps every token file on the fake's bearer.
func fixedBearer() *TokenFiles {
	return &TokenFiles{Token: func(context.Context) (string, error) { return "rs_test", nil }}
}

// A build session's MCP config names runsheet as the built-in, its
// tools/list reaches the platform's /mcp on that server, and its
// mcp__runsheet__mesh_finish is the finish the node records.
func TestRunsheetServerBuildSessionE2E(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	claudeBin, meshBin := runsheetBinaries(t)
	saw := t.TempDir()
	t.Setenv(runsheetE2EDir, saw)
	t.Setenv("RUNSHEET_E2E_ITEM", item1)

	f := seed(t)
	h := &harness.ClaudeCode{Binary: claudeBin, Unit: sessionunit.Detached{}, MeshBinary: meshBin, Tokens: fixedBearer()}
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	r.MCP = MeshContextMCP(meshBin, f.URL())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r.RunItem(ctx, item); err != nil {
		t.Fatalf("RunItem: %v\n%s", err, logs.String())
	}

	// the rendered config: runsheet is the built-in, the relay to /mcp
	var argv []string
	readJSON(t, filepath.Join(saw, "argv.json"), &argv)
	var mc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(flagValue(argv, "--mcp-config")), &mc); err != nil {
		t.Fatalf("--mcp-config: %v (argv %q)", err, argv)
	}
	srv, ok := mc.MCPServers["runsheet"]
	if !ok {
		t.Fatalf("mcpServers has no runsheet: %v", mc.MCPServers)
	}
	for _, alias := range tools.BuiltinAliases() {
		if _, ok := mc.MCPServers[alias]; ok {
			t.Errorf("mcpServers still names %s: %v", alias, mc.MCPServers)
		}
	}
	if srv.Command != meshBin || len(srv.Args) == 0 || srv.Args[0] != harness.RelayCommand ||
		flagValue(srv.Args, "--url") != MeshMCPURL(f.URL()) || filepath.Base(flagValue(srv.Args, "--token-file")) != harness.TokenFileName {
		t.Errorf("runsheet server = %+v, want %s %s to %s on its %s", srv, meshBin, harness.RelayCommand, MeshMCPURL(f.URL()), harness.TokenFileName)
	}
	if allowed := flagValue(argv, "--allowedTools"); !strings.Contains(allowed, "mcp__runsheet__*") {
		t.Errorf("--allowedTools = %q, want mcp__runsheet__*", allowed)
	}

	// the session's tools/list went out on runsheet and the fake took it
	var listed []string
	readJSON(t, filepath.Join(saw, "tools.json"), &listed)
	if !slices.Contains(listed, "mesh_document") || !slices.Contains(listed, "mesh_search") {
		t.Errorf("tools/list answered %v, want the context tools", listed)
	}
	reqs := f.MCPRequestsNamed("tools/list")
	if len(reqs) != 1 || reqs[0].Bearer != "rs_test" || reqs[0].Session == "" {
		t.Fatalf("the fake's tools/list requests = %+v, want one on the session's bearer and session", reqs)
	}
	if n := len(f.MCPRequestsNamed("initialize")); n != 1 {
		t.Errorf("initialize requests = %d, want 1", n)
	}

	// the finish by tool call, pushed then handed in
	finish := f.CallsNamed("finishWorkItem")
	if len(finish) != 1 || finish[0].Input["headSha"] != "abc123def456" || finish[0].Input["branch"] != "mesh/ship-it-1" {
		t.Fatalf("finishWorkItem = %+v\n%s", finish, logs.String())
	}
	if s := f.Item(item1)["status"]; s != "evaluating" {
		t.Fatalf("item status %v, want evaluating\n%s", s, logs.String())
	}
	if out := logs.String(); strings.Contains(out, "no mesh_finish call") {
		t.Fatalf("the mcp__runsheet__mesh_finish call was not taken as the finish:\n%s", out)
	}
	if len(ws.pushed) != 1 {
		t.Fatalf("pushed %v, want the branch once", ws.pushed)
	}
}

// A build session an older node started, whose state names its token
// file mesh-context.token and whose stream names the built-in
// mesh-context, is adopted and finishes: its mcp__mesh-context__mesh_finish
// is the finish.
func TestRunsheetServerAdoptedMeshContextSessionE2E(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	f := seed(t)
	cfg := registeredNode(t, f)
	root := t.TempDir()
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.ClaimWorkItem(context.Background(), item1, cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	const old = "mesh-context"
	if !tools.IsBuiltin(old) {
		t.Fatalf("%s is not read as the built-in", old)
	}
	output := runsheetFinishStream("s-old", old, item1)
	ws := deadSession(t, root, cfg.NodeID, output...)
	dir := sessionunit.Dir(ws)
	st, err := sessionunit.ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.TokenFile = filepath.Join(dir, old+".token")
	if err := sessionunit.WriteState(dir, st); err != nil {
		t.Fatal(err)
	}

	claude := &countedClaude{ClaudeCode: &harness.ClaudeCode{Binary: "/bin/false", Unit: sessionunit.Detached{}, Tokens: fixedBearer()}}
	loop, runner := adoptNode(t, f, cfg, root, claude)
	logs := &syncBuf{}
	runner.Log = log.New(logs, "", 0)
	found, err := FindSessions(root, []string{cfg.NodeID})
	if err != nil || len(found) != 1 || found[0].State.TokenFile != st.TokenFile {
		t.Fatalf("found %+v, %v", found, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if n := loop.AdoptSessions(ctx, found); n != 1 {
		t.Fatalf("adopted %d", n)
	}
	loop.wg.Wait()

	finish := f.CallsNamed("finishWorkItem")
	if len(finish) != 1 || finish[0].Input["headSha"] != "abc123def456" {
		t.Fatalf("finishWorkItem = %+v\n%s", finish, logs.String())
	}
	if s := f.Item(item1)["status"]; s != "evaluating" {
		t.Fatalf("item status %v, want evaluating\n%s", s, logs.String())
	}
	if out := logs.String(); strings.Contains(out, "no mesh_finish call") {
		t.Fatalf("the mcp__mesh-context__mesh_finish call was not taken as the finish:\n%s", out)
	}
	if claude.starts.Load() != 0 || len(f.CallsNamed("claimWorkItem")) != 1 {
		t.Fatal("the adopted session was run or claimed again")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir left after the finish: %v", err)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake claude left no %s: %v", filepath.Base(path), err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
