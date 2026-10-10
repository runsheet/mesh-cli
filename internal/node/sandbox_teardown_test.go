package node

import (
	"context"
	"errors"
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
)

// A sandboxed session's HOME, pod and containers end with it, whichever
// way it ends (goal a-sessions-temporary-home-is-removed-when-its-pod-ends,
// item 2): one test per path, each asserting no mesh-home-* directory is
// left in the sandbox's TempDir and that the Fake removed every pod it
// created. A node restart that adopts is the start-up sweep's (item 3).

// assertTornDown: at least one pod was made, every pod made was removed,
// and no session HOME is left under sb.TempDir.
func assertTornDown(t *testing.T, rt *sandbox.Fake, sb *sandbox.Sandbox) {
	t.Helper()
	if len(rt.Pods) == 0 {
		t.Fatalf("no pod was made: %v", rt.Calls)
	}
	for _, p := range rt.Pods {
		if !slices.Contains(rt.Removed, p.Name) {
			t.Fatalf("pod %s not removed: %v", p.Name, rt.Calls)
		}
	}
	homes, err := filepath.Glob(filepath.Join(sb.TempDir, "mesh-home-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(homes) != 0 {
		t.Fatalf("session HOMEs left: %v", homes)
	}
}

// teardownRunner is a Runner on the gateway fake whose builds run the
// real ClaudeCode harness in rt's pods. byNode says whether the node
// opens the session (OpenSessionPod, Runner.Sandbox set) or leaves it to
// the harness (job.Session nil, the harness's own Open and Close).
func teardownRunner(t *testing.T, rt *sandbox.Fake, byNode bool, yml string) (*Runner, *sandbox.Sandbox, gateway.WorkItem, *gatewaytest.Fake) {
	t.Helper()
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	sb.Config.ServiceTimeout = 2 * time.Second
	f := seed(t)
	r, ws, item, _ := runnerOnFake(t, f, &harness.ClaudeCode{Sandbox: sb}, nil)
	r.Declines = &Declines{}
	if byNode {
		r.Sandbox = sb
	}
	writeServices(t, filepath.Join(ws.root, "ship-it", "1"), yml)
	return r, sb, item, f
}

// streamAgent makes rt's agent write a successful session ending in text.
func streamAgent(rt *sandbox.Fake, text string) {
	rt.Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if spec.Stdin != nil {
			_, _ = io.Copy(io.Discard, spec.Stdin)
		}
		fmt.Fprintln(spec.Stdout, `{"type":"system","subtype":"init","session_id":"s-1"}`)
		fmt.Fprintf(spec.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":"s-1","total_cost_usd":0.5}`+"\n", text)
		return sandbox.Exit{}, nil
	}
}

// A build that finishes: the item is handed in and its pod and HOME go.
func TestSandboxTeardownBuildFinish(t *testing.T) {
	rt := &sandbox.Fake{}
	streamAgent(rt, "done")
	r, sb, item, f := teardownRunner(t, rt, true, servicesYML)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d", n)
	}
	assertTornDown(t, rt, sb)
}

// An evaluation that finishes: the verdict is recorded and its pod and
// HOME go.
func TestSandboxTeardownEvaluationFinish(t *testing.T) {
	rt := &sandbox.Fake{}
	streamAgent(rt, "The acceptance holds.\n\nVERDICT: PASS")
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	f := seed(t)
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	ws := &fakeWS{root: t.TempDir()}
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: &harness.ClaudeCode{Sandbox: sb}, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Sandbox: sb, Declines: &Declines{}, Log: log.New(os.Stderr, "", 0)}
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"
	if err := ev.Evaluate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if n := len(f.CallsNamed("evaluateWorkItem")); n != 1 {
		t.Fatalf("evaluateWorkItem calls = %d", n)
	}
	assertTornDown(t, rt, sb)
}

// A hand-back: a required service that does not come up releases the
// item before any agent starts, and the pod its sidecars were in and the
// HOME still go.
func TestSandboxTeardownHandBack(t *testing.T) {
	rt := &sandbox.Fake{Health: func(sidecar string, _ []string, _ int) error {
		if sidecar == "postgres" {
			return errors.New("refused")
		}
		return nil
	}}
	ran := false
	rt.Agent = func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) { ran = true; return sandbox.Exit{}, nil }
	r, sb, item, f := teardownRunner(t, rt, true, servicesYML)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if ran || len(f.CallsNamed("releaseWorkItem")) != 1 {
		t.Fatalf("ran=%v releases %v", ran, f.CallsNamed("releaseWorkItem"))
	}
	assertTornDown(t, rt, sb)
}

// A limit kill: the pod's memory limit ends the agent, the item is
// released as the node's condition, and the pod and HOME go.
func TestSandboxTeardownLimitKill(t *testing.T) {
	rt := &sandbox.Fake{}
	rt.Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		_, _ = io.Copy(io.Discard, spec.Stdin)
		return sandbox.Exit{Code: 137, OOMKilled: true}, nil
	}
	r, sb, item, f := teardownRunner(t, rt, true, "")
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatalf("a limit kill is not the item's error: %v", err)
	}
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 || !strings.Contains(fmt.Sprint(rel[0].Input["note"]), "memory") {
		t.Fatalf("releases: %+v", rel)
	}
	assertTornDown(t, rt, sb)
}

// A drain: the node's context is cancelled while the agent runs in its
// pod (a shutdown, a self-update); Run's removal and the node's Close
// still run, so the pod and HOME go.
func TestSandboxTeardownDrain(t *testing.T) {
	rt := &sandbox.Fake{}
	started := make(chan struct{})
	rt.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		_, _ = io.Copy(io.Discard, spec.Stdin)
		close(started)
		<-ctx.Done()
		return sandbox.Exit{Code: 137}, nil
	}
	r, sb, item, _ := teardownRunner(t, rt, true, servicesYML)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	_ = r.RunItem(ctx, item)
	select {
	case <-started:
	default:
		t.Fatalf("the agent never started: %v", rt.Calls)
	}
	assertTornDown(t, rt, sb)
}

// The harness's own session: with no session on the job (the node opened
// none) ClaudeCode opens one itself and closes it, pod and HOME.
func TestSandboxTeardownHarnessSession(t *testing.T) {
	rt := &sandbox.Fake{}
	streamAgent(rt, "done")
	r, sb, item, f := teardownRunner(t, rt, false, "")
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d", n)
	}
	assertTornDown(t, rt, sb)
}

// A validation with no session of its own to run in (an adopted build's
// pod ended with the node before it) opens one, and its close removes
// the step's pod and the HOME.
func TestSandboxTeardownValidationSession(t *testing.T) {
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte(validateYML), 0o644); err != nil {
		t.Fatal(err)
	}
	vr := validator{Sandbox: sb, logf: func(string, ...any) {},
		Inspect: func(context.Context, string, ...string) ([]byte, error) { return []byte("sha256:abc\n"), nil }}
	tgt := validationTarget{item: gateway.WorkItem{ID: item1}, dir: dir, prefix: "item ship-it/1", label: "ship-it/1"}
	v, err := vr.start(context.Background(), tgt)
	if err != nil || v == nil {
		t.Fatalf("start: %v %v", v, err)
	}
	if code, err := v.runner.Run(context.Background(), "true", time.Minute, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("step: %d %v", code, err)
	}
	v.close()
	assertTornDown(t, rt, sb)

}
