package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/tools"
)

// A Codex evaluation prepared the way Evaluator.Evaluate prepares one —
// an issued Codex subscription (PrepareCodexAccount), the account on the
// job, a console remote tool with a bearer secret beside the built-in
// from MeshContextMCP (SelectTools, UseTools), then Codex.Run — reaches a
// `codex` on PATH with the built-in's Authorization in its variable as
// `Bearer <the node token>`, its own CODEX_HOME and none of the
// operator's Codex variables, the console tool's bearer by
// bearer_token_env_var with its value in the environment, every server
// approved (exec refuses an unapproved MCP call under workspace-write),
// and no `${` anywhere: Codex expands none, so one would go out as a
// literal header (docs/codex-events.md, "Why the context tool failed in
// Codex").
func TestCodexEvaluationReceivesResolvedTools(t *testing.T) {
	cacheRoot(t)
	// the operator's own Codex login, which the session must not see
	t.Setenv("CODEX_HOME", "/home/operator/.codex")
	t.Setenv("OPENAI_API_KEY", "sk-operator")
	const (
		nodeToken    = "rs_FIXTURE-node-token-0001"
		consoleToken = "tok-FIXTURE-console-0001"
	)
	out, ws, bin := t.TempDir(), t.TempDir(), t.TempDir()
	script := "#!/bin/sh\n" +
		"cat > /dev/null\n" +
		"env > " + out + "/env\n" +
		"cp \"$CODEX_HOME/config.toml\" " + out + "/config.toml\n" +
		`echo '{"type":"thread.started","thread_id":"th-1"}'` + "\n" +
		`echo '{"type":"turn.started"}'` + "\n" +
		`echo '{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"VERDICT: PASS"}}'` + "\n" +
		`echo '{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}'` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	acct, err := PrepareCodexAccount(IssuedAccount(codexSub()), codexLogin)
	if err != nil {
		t.Fatal(err)
	}
	sa := &SessionAccount{AccountSession: acct}
	defer sa.Cleanup()
	builtin := MeshContextMCP("https://api.test", nodeToken)
	job := harness.Job{Kind: harness.KindEvaluate, Workspace: ws, Prompt: "evaluate", MCPServers: builtin, Model: "gpt-6-sol"}
	sa.Apply(&job)
	console := []tools.Tool{{Name: "search", Transport: tools.Remote, URL: "https://search.test/mcp",
		Secrets: map[string]string{"Authorization": "Bearer " + consoleToken}}}
	st := SelectTools(builtin, console, string(harness.KindEvaluate), nil)
	if len(st.Use) != 1 {
		t.Fatalf("tools in use %+v omitted %+v", st.Use, st.Omitted)
	}
	if err := st.UseTools(&job, builtin, sa); err != nil {
		t.Fatal(err)
	}
	res, err := (&harness.Codex{}).Run(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success || res.Text != "VERDICT: PASS" {
		t.Fatalf("result %+v", res)
	}

	raw := readTestFile(t, filepath.Join(out, "config.toml"))
	var cfg struct {
		MCPServers map[string]struct {
			URL               string            `toml:"url"`
			BearerTokenEnvVar string            `toml:"bearer_token_env_var"`
			EnvHTTPHeaders    map[string]string `toml:"env_http_headers"`
			ApprovalMode      string            `toml:"default_tools_approval_mode"`
		} `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	env := map[string]string{}
	for _, line := range strings.Split(readTestFile(t, filepath.Join(out, "env")), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	// the Unset list against job.Env: the account's home, not the
	// operator's, and no inherited key
	if home, _ := envOf(acct.Env, "CODEX_HOME"); env["CODEX_HOME"] != home {
		t.Fatalf("CODEX_HOME %q, want the account's %q", env["CODEX_HOME"], home)
	}
	if _, ok := env["OPENAI_API_KEY"]; ok {
		t.Fatal("the session sees the operator's OPENAI_API_KEY")
	}
	p, ok := cfg.MCPServers[tools.Builtin]
	if !ok || p.URL != "https://api.test/mcp" {
		t.Fatalf("the built-in %+v in\n%s", p, raw)
	}
	if v := p.EnvHTTPHeaders["Authorization"]; v == "" || env[v] != "Bearer "+nodeToken {
		t.Fatalf("the built-in's Authorization goes by %q, which holds %q", v, env[v])
	}
	s, ok := cfg.MCPServers["search"]
	if !ok || s.URL != "https://search.test/mcp" || s.BearerTokenEnvVar == "" {
		t.Fatalf("the console tool %+v in\n%s", s, raw)
	}
	if env[s.BearerTokenEnvVar] != consoleToken {
		t.Fatalf("the console tool's bearer goes by %q, which holds %q", s.BearerTokenEnvVar, env[s.BearerTokenEnvVar])
	}
	for name, srv := range cfg.MCPServers {
		if srv.ApprovalMode != tools.CodexApproval {
			t.Errorf("%s: default_tools_approval_mode %q", name, srv.ApprovalMode)
		}
	}
	if strings.Contains(raw, "${") {
		t.Errorf("config.toml holds an unexpanded reference:\n%s", raw)
	}
	for _, secret := range []string{nodeToken, consoleToken} {
		if strings.Contains(raw, secret) {
			t.Errorf("config.toml holds a secret value:\n%s", raw)
		}
	}
	for k, v := range env {
		if strings.Contains(v, "${") {
			t.Errorf("env %s holds an unexpanded reference: %q", k, v)
		}
	}
}

// Evaluator.Evaluate on a Codex family hands the preflight the very job
// it builds, and that job's servers carry the built-in, mesh-context, at
// the gateway's /mcp on the node's token — what Codex.Preflight checks
// before the session starts (goal evaluation-reports-no-verdict, item
// 2: the evaluation path is covered).
func TestCodexEvaluationPreflightsMeshContext(t *testing.T) {
	f, ev, _, codex, _, _ := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "Holds.\nVERDICT: PASS")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	pf := &fakePreflight{}
	ev.CodexPreflight = pf.check
	evaluateItem1(t, f, ev)
	if len(pf.jobs) != 1 || codex.runs() != 1 {
		t.Fatalf("preflights %d, codex runs %d", len(pf.jobs), codex.runs())
	}
	job := pf.jobs[0]
	if job.Kind != harness.KindEvaluate {
		t.Fatalf("kind %q", job.Kind)
	}
	srv, ok := job.MCPServers[tools.Builtin].(map[string]any)
	if !ok {
		t.Fatalf("the preflighted job's servers %v carry no %s", job.MCPServers, tools.Builtin)
	}
	if srv["url"] != strings.TrimRight(f.URL(), "/")+"/mcp" {
		t.Fatalf("url %v", srv["url"])
	}
	if h, _ := srv["headers"].(map[string]string); h["Authorization"] != "Bearer rs_test" {
		t.Fatalf("headers %v", srv["headers"])
	}
	if _, ok := codex.jobs[0].MCPServers[tools.Builtin]; !ok {
		t.Fatal("the Codex session's job has no mesh-context")
	}
}

// `prologue: not found` (goal evaluation-reports-no-verdict): the
// context tool answered every call with an error of its own — a
// mesh_document slug it does not have — so it was reached, and the
// session's verdict stands; nothing runs on the other family.
func TestAContextToolThatAnsweredIsAVerdict(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", "Missing the test.\nVERDICT: FAIL")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	codex.tools = map[string]*harness.ToolStat{"mesh-context": {Calls: 2, Errors: 2, Answered: 2, FirstError: "not found"}}
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 0 || in["passed"] != false {
		t.Fatalf("runs claude %d codex %d, recorded %+v", len(claude.jobs), codex.runs(), in)
	}
	if strings.Contains(logs.String(), "saw no context") {
		t.Fatalf("node.log:\n%s", logs.String())
	}
	// one call the tool never answered among them is still an answer
	f, ev, claude, codex, _, _ = familyFixture(t, "normal", "sonnet", "VERDICT: PASS", "Missing the test.\nVERDICT: FAIL")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	codex.tools = map[string]*harness.ToolStat{"mesh-context": {Calls: 2, Errors: 2, Answered: 1, FirstError: "timeout"}}
	if in := evaluateItem1(t, f, ev); len(claude.jobs) != 0 || in["passed"] != false {
		t.Fatalf("runs claude %d, recorded %+v", len(claude.jobs), in)
	}
	// none answered: no context, as before
	f, ev, claude, codex, _, _ = familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	codex.tools = map[string]*harness.ToolStat{"mesh-context": {Calls: 2, Errors: 2, FirstError: "MCP tool call requires approval, but approval policy is never"}}
	if in := evaluateItem1(t, f, ev); len(claude.jobs) != 1 || in["passed"] != true {
		t.Fatalf("runs claude %d, recorded %+v", len(claude.jobs), in)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
