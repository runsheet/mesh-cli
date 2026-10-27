//go:build !windows

package node

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// codexRefreshed is the login the pod's codex refreshes its auth.json to
// mid-session: what the adopting node writes back to the credential's
// login cache.
const codexRefreshed = `{"OPENAI_API_KEY":null,"tokens":{"id_token":"idt-FIXTURE-codex-id-token-0002","access_token":"at-FIXTURE-codex-access-token-0002","refresh_token":"rt-FIXTURE-codex-refresh-token-0002","account_id":"acct-1"},"last_refresh":"2026-10-17T00:00:00Z"}`

// codexPodAgent is a Codex evaluation's agent in the Fake's detached
// container: it says its thread started, refreshes the login in its
// CODEX_HOME, and waits for release (or its container's stop) before it
// gives its verdict by mesh_result and completes the turn.
func codexPodAgent(rt *sandbox.Fake) (started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	rt.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if spec.Stdin != nil {
			_, _ = io.Copy(io.Discard, spec.Stdin)
		}
		if len(spec.Command) < 2 || spec.Command[0] != "codex" || spec.Command[1] != "exec" {
			return sandbox.Exit{Code: 2}, nil
		}
		home := ""
		for _, kv := range spec.Env {
			if v, ok := strings.CutPrefix(kv, "CODEX_HOME="); ok {
				home = v
			}
		}
		if home == "" || os.WriteFile(filepath.Join(home, "auth.json"), []byte(codexRefreshed), 0o600) != nil {
			return sandbox.Exit{Code: 3}, nil
		}
		fmt.Fprintln(spec.Stdout, `{"type":"thread.started","thread_id":"th-pod"}`)
		fmt.Fprintln(spec.Stdout, `{"type":"turn.started"}`)
		closeOnce(started)
		select {
		case <-release:
		case <-ctx.Done():
			return sandbox.Exit{Code: 143}, nil
		}
		fmt.Fprintln(spec.Stdout, `{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"It holds."}}`)
		// the verdict goes to the node's mesh_result (ADR-041), on the
		// stream as the mcp_tool_call Codex records: the adopting node's
		// holder is fresh, so it is recovered from the stream
		fmt.Fprintln(spec.Stdout, `{"type":"item.completed","item":{"id":"i2","type":"mcp_tool_call","server":"mesh-node","tool":"mesh_result","status":"completed","arguments":{"verdict":{"passed":true,"notes":"It holds."}},"result":{"content":[{"type":"text","text":"recorded"}]}}}`)
		fmt.Fprintln(spec.Stdout, `{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":0,"output_tokens":50}}`)
		return sandbox.Exit{}, nil
	}
	return started, release
}

// codexPodNode is one node process on f whose evaluations run on Codex
// in their pods over sb: the Codex harness detaching a sandboxed
// session (DetachSandboxed, as node start sets it), the Claude harness
// beside it, and the adopter node start wires.
func codexPodNode(f *gatewaytest.Fake, cfg *Config, root string, sb *sandbox.Sandbox) (*Loop, *syncBuf) {
	gw := gateway.New(f.URL(), "rs_test")
	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	limits, known := &Limits{}, &KnownAccounts{}
	_ = known.Note(IssuedAccount(codexSub()), time.Now())
	codex := &harness.Codex{Sandbox: sb, DetachSandboxed: true}
	claude := &harness.ClaudeCode{Sandbox: sb, DetachSandboxed: true}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Harness: "claude-code", Log: logger, ClaimsDir: ClaimsDir(root),
		Known: known, Limits: limits, Dispatch: func(context.Context, gateway.WorkItem) {}}
	ev := &Evaluator{GW: gw, Harness: claude, WS: &fakeWS{root: root}, Scopes: scopesOf(cfg), WorkRoot: root, MaxBudgetUSD: 2, Model: "sonnet",
		Sandbox: sb, Codex: ObserveLimits(codex, limits), CodexModel: CodexConfig{}.CodexModel,
		CodexAccounts: loop.CodexCandidates, Waits: &OncePer{}, Limits: limits, Thresholds: func() LimitsConfig { return LimitsConfig{} },
		Credentials: &Credentials{GW: gw, Limits: limits, Known: known, Log: logger}, Log: logger}
	loop.Evaluate = func(ctx context.Context, item gateway.WorkItem) { _ = ev.Evaluate(ctx, item) }
	loop.AdoptSession = AdoptWith{GW: gw, Scopes: loop.CurrentScopes, Logf: logger.Printf, Evaluate: ev.AdoptEvaluation}.Adopt
	return loop, logs
}

// Goal codex-sessions-run-in-the-sandbox, item 4: a Codex evaluation on
// a sandboxed node runs detached in its pod, outlives the node's
// Restart — its state naming adapter codex, the pod, the container and
// the HOME, and no secret — and the next process finds it, follows the
// container, parses the Codex stream and records its verdict exactly
// once, writes the login the session refreshed back to the
// credential's cache, then removes the pod and HOME.
func TestCodexPodSessionAdopted(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	cache := cacheRoot(t)
	f, cfg := evalFake(t)
	f.SessionCredentials[item1] = []map[string]any{
		{"id": "cred-1", "label": "max-cloud", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": issuedSecret},
		{"id": "oa-1", "label": "chatgpt-pro", "vendor": "openai", "kind": "oauth_token", "serves": []any{"codex"}, "secret": codexLogin, "secretVersion": float64(1)},
	}
	rt := &sandbox.Fake{}
	started, release := codexPodAgent(rt)
	t.Cleanup(func() { closeOnce(release) })
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	sb.Config.ServiceTimeout = 2 * time.Second
	root := t.TempDir()

	// the first process: its tick starts the Codex evaluation in a pod
	loop1, logs1 := codexPodNode(f, cfg, root, sb)
	loop1.Tick(loop1.Context(context.Background()))
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatalf("the Codex agent never started: %v\n%s", rt.Calls, logs1)
	}
	var found []FoundSession
	waitFor(t, func() bool {
		found, _ = FindSessionsIn(root, []string{cfg.NodeID}, sb)
		return len(found) == 1 && found[0].State.Offset > 0
	})
	s := found[0]
	loop1.Restart()
	loop1.wg.Wait()

	// left running: the container, its pod and HOME, the state naming them
	if !s.Sandboxed() || !s.Alive() || len(rt.StoppedAgents()) != 0 || slices.Contains(rt.Removed, s.State.Pod) {
		t.Fatalf("the Codex session did not outlive the node: sandboxed %v alive %v, stopped %v, removed %v\n%s", s.Sandboxed(), s.Alive(), rt.StoppedAgents(), rt.Removed, logs1)
	}
	if _, err := os.Stat(s.State.Home); err != nil {
		t.Fatalf("the session's HOME went with the node: %v", err)
	}
	if st := s.State; st.Harness != AdapterCodex || st.Kind != string(harness.KindEvaluate) || st.Runner != sessionunit.SandboxRunner ||
		st.Pod == "" || st.Container == "" || !strings.HasPrefix(st.Home, sb.TempDir) || st.Model != "gpt-6-luna" ||
		st.LoginCache != CodexLoginCache(cache, "oa-1", 1) || st.LoginFile == "" {
		t.Fatalf("state %+v", st)
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir, sessionunit.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"at-FIXTURE-codex-access-token-0001", codexRefresh, issuedSecret} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("state.json carries a secret: %s", raw)
		}
	}
	if n := evaluateJobsClosed(f); n != 0 {
		t.Fatalf("an evaluation closed before the restart: %d", n)
	}

	// the next process finds it, keeps its pod through the sweep, adopts
	loop2, logs2 := codexPodNode(f, cfg, root, sb)
	found, err = FindSessionsIn(root, []string{cfg.NodeID}, sb)
	if err != nil || len(found) != 1 || found[0].State.Container != s.State.Container || !found[0].Alive() {
		t.Fatalf("found %+v, %v", found, err)
	}
	SweepSandbox(context.Background(), sb, found, func(string, ...any) {})
	ctx2, stop2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop2()
	if n := loop2.AdoptSessions(ctx2, found); n != 1 {
		t.Fatalf("adopted %d\n%s", n, logs2)
	}
	loop2.Tick(ctx2) // the adopted evaluation is in flight: none started again
	time.Sleep(100 * time.Millisecond)
	// the adopted session hands its PASS to the product's tool, which
	// takes the item out of evaluating
	f.HandedInVerdicts = map[string]map[string]any{item1: {"passed": true, "notes": "Holds."}}
	closeOnce(release)
	waitFor(t, func() bool { return evaluateJobsClosed(f) >= 1 })
	loop2.wg.Wait()
	loop2.Tick(ctx2)
	loop2.wg.Wait()

	// one evaluateWorkItem: the product applying the session's PASS
	// (HandedInVerdicts), on the one close the node sent
	if n := len(f.CallsNamed("evaluateWorkItem")); n != 1 {
		t.Fatalf("evaluateWorkItem calls: %d\n%s", n, logs2)
	}
	if notes, _ := evaluationClosed(t, f)["notes"].(string); !strings.Contains(notes, "(evaluated on Codex gpt-6-luna)") {
		t.Fatalf("notes tail %q does not name Codex", notes)
	}
	if n := len(rt.Detached); n != 1 {
		t.Fatalf("%d detached agents, want the one adopted", n)
	}
	if !strings.Contains(logs2.String(), "adopted its") || !strings.Contains(logs2.String(), "evaluation on Codex gpt-6-luna") {
		t.Fatalf("the adoption is not logged on Codex:\n%s", logs2)
	}
	if !slices.Contains(rt.Removed, s.State.Pod) {
		t.Fatalf("the session's pod %s was not removed: %v", s.State.Pod, rt.Calls)
	}
	if _, err := os.Stat(s.State.Home); !os.IsNotExist(err) {
		t.Fatalf("the session's HOME %s is left: %v", s.State.Home, err)
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatalf("the session directory %s is left: %v", s.Dir, err)
	}
	if got, err := os.ReadFile(CodexLoginCache(cache, "oa-1", 1)); err != nil || string(got) != codexRefreshed {
		t.Fatalf("the refreshed login was not kept for the credential: %q %v", got, err)
	}
}
