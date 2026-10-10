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
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// servicesYML is a repository's .mesh/services.yml: Postgres required,
// Redis not, each with a health check the Fake answers.
const servicesYML = `services:
  postgres:
    image: postgres:16
    env:
      POSTGRES_PASSWORD: postgres
    ports: ["5432:5432"]
    options: --health-cmd "pg_isready -U postgres" --health-interval 10ms --health-retries 3
  redis:
    image: redis:7
    ports: ["6379"]
    required: false
`

// agentSeen is what the Fake's agent container saw: its prompt, its
// $HOME/.mesh-services and its env.
type agentSeen struct {
	mu       sync.Mutex
	prompt   string
	services string
	ran      bool
}

// servicesRunner is a Runner on the gateway fake whose sessions run the
// real ClaudeCode harness in the Fake runtime's pods, on a checkout
// declaring yml. exit is how the agent's container ends (nil: a
// successful session's stream).
func servicesRunner(t *testing.T, yml string, rt *sandbox.Fake, exit func(ctx context.Context) (sandbox.Exit, error)) (*Runner, gateway.WorkItem, *gatewaytest.Fake, *agentSeen) {
	t.Helper()
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	sb.Config.ServiceTimeout = 2 * time.Second
	f := seed(t)
	r, ws, item, _ := runnerOnFake(t, f, &harness.ClaudeCode{Sandbox: sb}, nil)
	r.Sandbox, r.Declines = sb, &Declines{}
	writeServices(t, filepath.Join(ws.root, "ship-it", "1"), yml)
	return r, item, f, fakeAgent(rt, exit)
}

func writeServices(t *testing.T, dir, yml string) {
	t.Helper()
	if yml == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(dir, ".mesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sandbox.ServicesFile), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeAgent makes rt's agent container read its prompt and services
// file, then end as exit says.
func fakeAgent(rt *sandbox.Fake, exit func(ctx context.Context) (sandbox.Exit, error)) *agentSeen {
	seen := &agentSeen{}
	rt.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		prompt, _ := io.ReadAll(spec.Stdin)
		home, _ := envOf(spec.Env, "HOME")
		svc, _ := os.ReadFile(filepath.Join(home, sandbox.ServicesFileName))
		seen.mu.Lock()
		seen.prompt, seen.services, seen.ran = string(prompt), string(svc), true
		seen.mu.Unlock()
		if exit != nil {
			return exit(ctx)
		}
		fmt.Fprintln(spec.Stdout, `{"type":"system","subtype":"init","session_id":"s-1"}`)
		fmt.Fprintln(spec.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s-1","total_cost_usd":0.5}`)
		return sandbox.Exit{}, nil
	}
	return seen
}

func callIndex(t *testing.T, rt *sandbox.Fake, prefix string) int {
	t.Helper()
	i, ok := rt.Called(prefix)
	if !ok {
		t.Fatalf("no %q call: %v", prefix, rt.Calls)
	}
	return i
}

func notes(f *gatewaytest.Fake) string {
	var out []string
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		out = append(out, fmt.Sprint(c.Input["note"]))
	}
	return strings.Join(out, "\n")
}

// The services are sidecars in the agent's own pod, started and
// health-checked before the agent — which starts only once the checks
// pass — under the services' budget split between them; the prompt and
// $HOME/.mesh-services list them on 127.0.0.1; the pod goes at the end.
func TestServicesStartInThePodBeforeTheAgent(t *testing.T) {
	pgChecks := 0
	rt := &sandbox.Fake{Health: func(sidecar string, cmd []string, port int) error {
		if sidecar == "postgres" {
			pgChecks++
			if pgChecks < 3 {
				return errors.New("no response")
			}
		}
		return nil
	}}
	r, item, f, seen := servicesRunner(t, servicesYML, rt, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if st := f.Item(item1)["status"]; st != "evaluating" && st != "finished" {
		t.Fatalf("status = %v", st)
	}
	if len(rt.Pods) != 1 {
		t.Fatalf("pods: %+v", rt.Pods)
	}
	pod := rt.Pods[0].Name
	agent := callIndex(t, rt, "agent "+pod)
	for _, c := range []string{"create " + pod, "sidecar " + pod + " postgres", "sidecar " + pod + " redis", "health " + pod + " postgres", fmt.Sprintf("probe %s 6379", pod)} {
		if i := callIndex(t, rt, c); i > agent {
			t.Errorf("%q came after the agent: %v", c, rt.Calls)
		}
	}
	// the agent waited for the third check, the first that passed
	var healthBefore int
	for _, c := range rt.Calls[:agent] {
		if c == "health "+pod+" postgres" {
			healthBefore++
		}
	}
	if healthBefore != 3 {
		t.Errorf("postgres checks before the agent = %d, want 3: %v", healthBefore, rt.Calls)
	}
	if rt.Calls[len(rt.Calls)-1] != "remove "+pod {
		t.Errorf("the pod is removed last: %v", rt.Calls)
	}
	pg := rt.Sidecars[slices.IndexFunc(rt.Sidecars, func(s sandbox.SidecarSpec) bool { return s.Name == "postgres" })]
	if pg.Limits != (sandbox.Limits{CPUs: 1, Memory: "2048m", Pids: sandbox.SidecarPids}) {
		t.Errorf("postgres limits = %+v (half of 2 cpus and 4g each)", pg.Limits)
	}
	if !slices.Equal(pg.Env, []string{"POSTGRES_PASSWORD=postgres"}) || !slices.Equal(pg.Command, sandbox.PostgresTuning) {
		t.Errorf("postgres = %+v", pg)
	}
	const line = "# Services\n\nThe services this repository declares run beside you and are ready on 127.0.0.1 (also listed in `$HOME/.mesh-services`): postgres (postgres:16) at 127.0.0.1:5432; redis (redis:7) at 127.0.0.1:6379.\n\n# Rules\n"
	if !strings.Contains(seen.prompt, line) {
		t.Errorf("prompt lacks\n%s\n---\n%s", line, seen.prompt)
	}
	if want := "postgres\tpostgres:16\t127.0.0.1:5432\tready\nredis\tredis:7\t127.0.0.1:6379\tready\n"; seen.services != want {
		t.Errorf(".mesh-services = %q, want %q", seen.services, want)
	}
}

// A required service that does not come up hands the claim back with a
// note naming it and why; the agent never starts, the pod is removed
// and the item is not taken again while that holds.
func TestRequiredServiceFailingReleasesTheItem(t *testing.T) {
	rt := &sandbox.Fake{Health: func(sidecar string, cmd []string, port int) error {
		if sidecar == "postgres" {
			return errors.New("pg_isready: no response")
		}
		return nil
	}}
	r, item, f, seen := servicesRunner(t, servicesYML, rt, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatalf("a failed service is not the item's error: %v", err)
	}
	if seen.ran {
		t.Fatal("the agent must not start")
	}
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 {
		t.Fatalf("releases: %+v", rel)
	}
	if reason := fmt.Sprint(rel[0].Input["note"]); reason != "services: required service postgres (postgres:16) did not come up: health check failed 3 times: pg_isready: no response" {
		t.Fatalf("reason = %q", reason)
	}
	if len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatal("the item is released, not failed")
	}
	if len(rt.Removed) != 1 || rt.Removed[0] != rt.Pods[0].Name {
		t.Fatalf("pod not removed: %v", rt.Calls)
	}
	if !r.Declines.Holds(item.ID, nil, time.Now()) {
		t.Fatal("the node must not take the item again straight away")
	}
}

// A service that is not required and does not come up is a progress
// note naming it and why; the agent is told it is missing and starts.
func TestOptionalServiceFailingIsReportedAndTheAgentStarts(t *testing.T) {
	rt := &sandbox.Fake{SidecarErr: map[string]error{"redis": errors.New("podman run redis: pull access denied")}}
	r, item, f, seen := servicesRunner(t, servicesYML, rt, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if !seen.ran {
		t.Fatal("the agent must start")
	}
	if want := "services missing (not required; the session goes on without them): redis (redis:7): did not start: podman run redis: pull access denied"; !strings.Contains(notes(f), want) {
		t.Fatalf("notes lack %q:\n%s", want, notes(f))
	}
	if want := "The services this repository declares run beside you and are ready on 127.0.0.1 (also listed in `$HOME/.mesh-services`): postgres (postgres:16) at 127.0.0.1:5432. Missing — it could not start, so a test that needs it fails here for that reason, not your code's: redis (redis:7): did not start: podman run redis: pull access denied.\n"; !strings.Contains(seen.prompt, want) {
		t.Fatalf("prompt lacks %q:\n%s", want, seen.prompt)
	}
	if len(f.CallsNamed("releaseWorkItem")) != 0 {
		t.Fatal("an optional service does not release the item")
	}
}

// A service from a registry outside the allow-list is refused before
// any pull, with a note naming the image and the list; a required one
// releases the claim without a pod.
func TestDisallowedRegistryIsRefusedWithANote(t *testing.T) {
	rt := &sandbox.Fake{}
	r, item, f, seen := servicesRunner(t, "services:\n  db:\n    image: registry.example.com/pg:16\n    ports: [5432]\n", rt, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if seen.ran || len(rt.Pods) != 0 || len(rt.Sidecars) != 0 {
		t.Fatalf("nothing is pulled or started: %v", rt.Calls)
	}
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 {
		t.Fatalf("releases: %+v", rel)
	}
	want := "services: required service db (registry.example.com/pg:16) did not come up: image registry.example.com/pg:16 refused: registry registry.example.com is not allowed (allowed: docker.io, ghcr.io, *.pkg.dev, quay.io)"
	if reason := fmt.Sprint(rel[0].Input["note"]); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}

	// not required: noted, and the session goes on without it
	rt = &sandbox.Fake{}
	r, item, f, seen = servicesRunner(t, "services:\n  db:\n    image: registry.example.com/pg:16\n    required: false\n", rt, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if !seen.ran || len(rt.Sidecars) != 0 || !strings.Contains(notes(f), "db (registry.example.com/pg:16): image registry.example.com/pg:16 refused: registry registry.example.com is not allowed") {
		t.Fatalf("ran=%v sidecars=%v notes:\n%s", seen.ran, rt.Sidecars, notes(f))
	}
}

// The pod and its sidecars are removed whatever way the session ends:
// success, failure, a limit kill, a timeout and the node stopping.
func TestServicesPodRemovedOnEveryExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		exit func(ctx context.Context) (sandbox.Exit, error)
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{name: "success"},
		{name: "failure", exit: func(context.Context) (sandbox.Exit, error) { return sandbox.Exit{Code: 1}, nil }},
		{name: "limit kill", exit: func(context.Context) (sandbox.Exit, error) { return sandbox.Exit{Code: 137, OOMKilled: true}, nil }},
		{name: "node stopping", exit: func(ctx context.Context) (sandbox.Exit, error) { <-ctx.Done(); return sandbox.Exit{Code: 137}, nil },
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 300*time.Millisecond)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &sandbox.Fake{}
			r, item, _, seen := servicesRunner(t, servicesYML, rt, tc.exit)
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
			}
			defer cancel()
			_ = r.RunItem(ctx, item)
			if !seen.ran {
				t.Fatalf("the agent did not run: %v", rt.Calls)
			}
			assertPodsGone(t, rt, r.Sandbox)
		})
	}

	t.Run("timeout", func(t *testing.T) {
		rt := &sandbox.Fake{}
		sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
		f := seed(t)
		f.WorkItems[item1]["status"] = "evaluating"
		f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
		gw := gateway.New(f.URL(), "rs_test")
		cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
		ws := &fakeWS{root: t.TempDir()}
		writeServices(t, EvalWorkspaceSpec(ws.root, "ship-it", 1, "", "", "").Dir(), servicesYML)
		seen := fakeAgent(rt, func(ctx context.Context) (sandbox.Exit, error) { <-ctx.Done(); return sandbox.Exit{Code: 137}, nil })
		ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: &harness.ClaudeCode{Sandbox: sb}, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Timeout: 200 * time.Millisecond, Sandbox: sb, Log: log.New(os.Stderr, "", 0)}
		var item gateway.WorkItem
		item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"
		err := ev.Evaluate(context.Background(), item)
		if err == nil || !strings.Contains(err.Error(), "ran out of time") {
			t.Fatalf("err = %v", err)
		}
		if !seen.ran || !strings.Contains(seen.prompt, "# Services\n\n") {
			t.Fatalf("the evaluator ran with its services: %v", rt.Calls)
		}
		assertPodsGone(t, rt, sb)
	})
}

// assertPodsGone: every pod made was removed with its sidecars, and no
// session HOME is left.
func assertPodsGone(t *testing.T, rt *sandbox.Fake, sb *sandbox.Sandbox) {
	t.Helper()
	if len(rt.Pods) != 1 || len(rt.Sidecars) != 2 {
		t.Fatalf("pods %d, sidecars %d: %v", len(rt.Pods), len(rt.Sidecars), rt.Calls)
	}
	if !slices.Contains(rt.Removed, rt.Pods[0].Name) {
		t.Fatalf("pod %s not removed: %v", rt.Pods[0].Name, rt.Calls)
	}
	left, _ := os.ReadDir(sb.TempDir)
	if len(left) != 0 {
		t.Fatalf("session HOMEs left: %v", left)
	}
}

// A required service that does not come up for an evaluation leaves the
// item unevaluated (it cannot be released) and held off, and a rebase
// gives up naming it; neither agent starts.
func TestRequiredServiceFailingEvaluateAndRebase(t *testing.T) {
	failing := func(sidecar string, cmd []string, port int) error {
		if sidecar == "postgres" {
			return errors.New("refused")
		}
		return nil
	}
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"

	rt := &sandbox.Fake{Health: failing}
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	f := seed(t)
	f.WorkItems[item1]["status"] = "evaluating"
	ws := &fakeWS{root: t.TempDir()}
	writeServices(t, EvalWorkspaceSpec(ws.root, "ship-it", 1, "", "", "").Dir(), servicesYML)
	seen := fakeAgent(rt, nil)
	ev := &Evaluator{Credentials: testCreds(gateway.New(f.URL(), "rs_test")), GW: gateway.New(f.URL(), "rs_test"), Harness: &harness.ClaudeCode{Sandbox: sb}, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Sandbox: sb, Declines: &Declines{}}
	err := ev.Evaluate(context.Background(), item)
	if err == nil || !strings.Contains(err.Error(), "required service postgres (postgres:16) did not come up") || seen.ran {
		t.Fatalf("evaluate: %v ran=%v", err, seen.ran)
	}
	if len(f.CallsNamed("evaluateWorkItem")) != 0 || !ev.Declines.Holds(item1, nil, time.Now()) {
		t.Fatalf("evaluate: verdicts %v", f.CallsNamed("evaluateWorkItem"))
	}
	assertPodsGone(t, rt, sb)

	rt = &sandbox.Fake{Health: failing}
	sb = &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	f = seed(t)
	f.WorkItems[item1]["status"] = "review"
	f.WorkItems[item1]["rebaseState"] = "claimed"
	f.RebaseTokens = true
	ws = &fakeWS{root: t.TempDir()}
	writeServices(t, filepath.Join(ws.root, "ship-it", "1"), servicesYML)
	seen = fakeAgent(rt, nil)
	rb := &Rebaser{Credentials: testCreds(gateway.New(f.URL(), "rs_test")), GW: gateway.New(f.URL(), "rs_test"), Harness: &harness.ClaudeCode{Sandbox: sb}, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Sandbox: sb}
	if err := rb.Rebase(context.Background(), item); err == nil || seen.ran {
		t.Fatalf("rebase: %v ran=%v", err, seen.ran)
	}
	fails := f.CallsNamed("failWorkItemRebase")
	if len(fails) != 1 || !strings.Contains(fmt.Sprint(fails[0].Input["reason"]), "required service postgres (postgres:16) did not come up") {
		t.Fatalf("rebase fails: %+v", fails)
	}
	assertPodsGone(t, rt, sb)
}

// Every sandboxed session logs, before its services and its agent, the
// image ref its agent runs: `sandbox: session <label> — image <ref>`.
func TestOpenSessionPodLogsTheImage(t *testing.T) {
	const ref = "ghcr.io/runsheet/mesh-node:v0.2.335"
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v0.2.335"), TempDir: t.TempDir()}
	sb.Config.Image = ref
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	pod, err := OpenSessionPod(context.Background(), sb, t.TempDir(), "ship-it#1", harness.KindBuild, sandbox.GitIdentity{}, logf)
	if err != nil {
		t.Fatal(err)
	}
	defer pod.Close()
	want := "sandbox: session ship-it#1 — image " + ref
	var found int
	for _, l := range lines {
		if strings.Contains(l, "ship-it#1") && strings.Contains(l, ref) {
			found++
			if l != want {
				t.Fatalf("line = %q, want %q", l, want)
			}
		}
	}
	if found != 1 {
		t.Fatalf("lines naming the session and its image: %d, want 1: %q", found, lines)
	}
}

// A Close that cannot remove what the session left is logged through
// the node's logf, `sandbox: session <label>: close: <err>`, never dropped.
func TestSessionPodCloseLogsAFailure(t *testing.T) {
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	pod, err := OpenSessionPod(context.Background(), sb, t.TempDir(), "ship-it#1", harness.KindBuild, sandbox.GitIdentity{}, logf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pod.Session.Start(context.Background(), nil, 0); err != nil {
		t.Fatal(err)
	}
	rt.RemoveErr = errors.New("podman pod: pod is busy")
	lines = nil
	pod.Close()
	if len(lines) != 1 || lines[0] != "sandbox: session ship-it#1: close: podman pod: pod is busy" {
		t.Fatalf("lines = %q", lines)
	}
}
