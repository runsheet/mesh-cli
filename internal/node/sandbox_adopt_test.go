//go:build !windows

package node

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
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
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// Goal a-sandboxed-session-outlives-the-node, item 3: a sandboxed
// session is found, adopted and finished on start like a host one, and
// the start-up sweep and the capacity probe leave it to its adopter.

// detachedAgent is a sandboxed build's agent in the Fake's detached
// container: it commits a file in the checkout, says it started, and
// waits for release (or its container's stop) before it ends the
// session. A validation step's container (sh -c) passes at once.
type detachedAgent struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newDetachedAgent(rt *sandbox.Fake) *detachedAgent {
	a := &detachedAgent{started: make(chan struct{}), release: make(chan struct{})}
	rt.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if len(spec.Command) > 0 && spec.Command[0] == "sh" {
			return sandbox.Exit{}, nil // a validation step
		}
		if spec.Stdin != nil {
			_, _ = io.Copy(io.Discard, spec.Stdin)
		}
		git := func(args ...string) error {
			cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
			cmd.Dir = spec.Workdir
			return cmd.Run()
		}
		if err := os.WriteFile(filepath.Join(spec.Workdir, "api", "api.go"), []byte("package api\n\nconst X = 1\n"), 0o644); err != nil {
			return sandbox.Exit{Code: 1}, nil
		}
		if git("add", "-A") != nil || git("commit", "--quiet", "-m", "the change") != nil {
			return sandbox.Exit{Code: 1}, nil
		}
		fmt.Fprintln(spec.Stdout, `{"type":"system","subtype":"init","session_id":"s-pod"}`)
		fmt.Fprintln(spec.Stdout, `{"type":"assistant","session_id":"s-pod","message":{"content":[{"type":"text","text":"committed"}]}}`)
		a.once.Do(func() { close(a.started) })
		select {
		case <-a.release:
		case <-ctx.Done():
			return sandbox.Exit{Code: 143}, nil
		}
		fmt.Fprintln(spec.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":"Committed.","session_id":"s-pod","total_cost_usd":0.5}`)
		return sandbox.Exit{}, nil
	}
	return a
}

// sandboxNode is one node process on f with a sandbox on rt: its loop
// and runner on the shared work root, a real checkout of origin, the
// adopter the node wires and a live probe counting by container.
func sandboxNode(t *testing.T, f *gatewaytest.Fake, cfg *Config, root, origin string, sb *sandbox.Sandbox, logs *syncBuf) (*Loop, *Runner) {
	t.Helper()
	gw := gateway.New(f.URL(), "rs_test")
	logger := log.New(logs, "", 0)
	h := &harness.ClaudeCode{Sandbox: sb, DetachSandboxed: true}
	ws := &gitWS{fakeWS: &fakeWS{root: root}, t: t, origin: origin}
	runner := &Runner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: root, ProgressEvery: 50 * time.Millisecond, Sandbox: sb, Declines: &Declines{},
		Inspect: func(context.Context, string, ...string) ([]byte, error) { return []byte("sha256:abc\n"), nil }, Log: logger}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: logger,
		Dispatch: func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }}
	loop.AdoptSession = AdoptWith{GW: gw, Scopes: loop.CurrentScopes, Logf: logger.Printf, Build: runner.AdoptItem}.Adopt
	loop.Live = &LiveProbe{Root: root, Sandbox: sb, SweepEvery: 1}
	return loop, runner
}

// startSandboxedBuild runs a first node until its sandboxed build's
// agent has committed and spoken, then cancels it: the session it
// leaves, found by a node of the same sandbox.
func startSandboxedBuild(t *testing.T) (f *gatewaytest.Fake, cfg *Config, rt *sandbox.Fake, sb *sandbox.Sandbox, agent *detachedAgent, root, origin string, found FoundSession) {
	t.Helper()
	sessionunit.PollInterval = 20 * time.Millisecond
	f = seed(t)
	cfg = registeredNode(t, f)
	rt = &sandbox.Fake{}
	agent = newDetachedAgent(rt)
	sb = &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	sb.Config.ServiceTimeout = 2 * time.Second
	root = t.TempDir()
	origin = originRepo(t, map[string]string{".runsheet/validate.yml": validateYML, sandbox.ServicesFile: servicesYML})

	loop1, _ := sandboxNode(t, f, cfg, root, origin, sb, &syncBuf{})
	ctx1, stop1 := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- loop1.Run(ctx1) }()
	select {
	case <-agent.started:
	case <-time.After(10 * time.Second):
		t.Fatalf("the agent never started: %v", rt.Calls)
	}
	var all []FoundSession
	waitFor(t, func() bool {
		all, _ = FindSessionsIn(root, []string{cfg.NodeID}, sb)
		return len(all) == 1 && all[0].State.Offset > 0
	})
	stop1()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("loop 1's Run did not return")
	}
	t.Cleanup(func() { closeOnce(agent.release) })
	return f, cfg, rt, sb, agent, root, origin, all[0]
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// A sandboxed build outlives its node and is finished by the next: the
// first loop's cancel leaves the pod, its sidecars and HOME; the next
// process finds the session, keeps its pod through the start-up sweep,
// heartbeats the item before its first tick, counts the session once
// and stops nothing; on the agent's exit it validates, pushes and
// finishes the item, then removes the pod and HOME.
func TestSandboxedBuildIsAdoptedAndFinished(t *testing.T) {
	f, cfg, rt, sb, agent, root, origin, s := startSandboxedBuild(t)

	// the first node let go: nothing of the session was removed
	if s.State.Runner != sessionunit.SandboxRunner || s.State.Pod == "" || s.State.Container == "" || s.State.Home == "" {
		t.Fatalf("state = %+v", s.State)
	}
	if len(rt.Removed) != 0 || len(rt.RemovedContainers) != 0 || len(rt.StoppedAgents()) != 0 {
		t.Fatalf("the cancel removed %v, containers %v, stopped %v", rt.Removed, rt.RemovedContainers, rt.StoppedAgents())
	}
	if len(rt.Sidecars) != 2 {
		t.Fatalf("sidecars = %+v, want postgres and redis", rt.Sidecars)
	}
	if _, err := os.Stat(s.State.Home); err != nil {
		t.Fatalf("the session's HOME went with the node: %v", err)
	}
	if !s.Alive() {
		t.Fatal("the session's container died with its node")
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 0 {
		t.Fatalf("finished before the restart: %d", n)
	}

	// the next process: found, swept around, adopted
	logs := &syncBuf{}
	loop2, _ := sandboxNode(t, f, cfg, root, origin, sb, logs)
	found, err := FindSessionsIn(root, []string{cfg.NodeID}, sb)
	if err != nil || len(found) != 1 || !found[0].Sandboxed() || !found[0].Alive() {
		t.Fatalf("found %+v, %v", found, err)
	}
	SweepSandbox(context.Background(), sb, found, func(string, ...any) {})
	if slices.Contains(rt.Removed, s.State.Pod) {
		t.Fatalf("the start-up sweep removed the adopted session's pod: %v", rt.Calls)
	}
	beforeAdopt := len(f.Calls)
	ctx2, stop2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop2()
	if n := loop2.AdoptSessions(ctx2, found); n != 1 {
		t.Fatalf("adopted %d", n)
	}
	heartbeat := false
	for _, c := range f.Calls[beforeAdopt:] {
		if c.Name == "reportWorkItemProgress" && c.Input["aggregateId"] == item1 && strings.HasPrefix(fmt.Sprint(c.Input["note"]), "session adopted by the restarted node (running)") {
			heartbeat = true
		}
		if c.Name == "heartbeatNode" || c.Name == "claimWorkItem" {
			t.Fatalf("%s before the adopted heartbeat", c.Name)
		}
	}
	if !heartbeat {
		t.Fatalf("no adopted heartbeat for the item: %v", f.CallsNamed("reportWorkItemProgress"))
	}

	// the capacity probe: the adopted session is the node's own, once
	if used := loop2.inUse(ctx2); used != 1 {
		t.Fatalf("inUse = %d, want 1", used)
	}
	if live, orphans := loop2.LiveSessions(); live != 1 || orphans != 0 {
		t.Fatalf("live %d, orphans %d", live, orphans)
	}
	if stopped := rt.StoppedAgents(); len(stopped) != 0 {
		t.Fatalf("the orphan sweep stopped %v", stopped)
	}

	// the agent ends: validated, pushed, finished, then the pod goes
	closeOnce(agent.release)
	waitFor(t, func() bool { return len(f.CallsNamed("finishWorkItem")) == 1 })
	loop2.wg.Wait()
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 1 {
		t.Fatalf("attestations = %d\n%s", n, logs)
	}
	if n := len(f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("claimWorkItem calls = %d, want the first node's alone", n)
	}
	steps := 0
	for _, a := range rt.Agents {
		if len(a.Command) > 0 && a.Command[0] == "sh" {
			steps++
		}
	}
	if steps == 0 {
		t.Fatalf("no validation step ran in a pod: %+v", rt.Agents)
	}
	if !slices.Contains(rt.Removed, s.State.Pod) {
		t.Fatalf("the session's pod %s was not removed: %v", s.State.Pod, rt.Calls)
	}
	if lastCall(rt, "remove "+s.State.Pod) < lastCall(rt, "agent ") {
		t.Fatalf("the pod went before the validation ran: %v", rt.Calls)
	}
	if _, err := os.Stat(s.State.Home); !os.IsNotExist(err) {
		t.Fatalf("the session's HOME %s is left: %v", s.State.Home, err)
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatalf("the session dir is left: %v", err)
	}
	if row := f.Item(item1); row["status"] != "evaluating" || row["branch"] != "mesh/ship-it-1" {
		t.Fatalf("item row %v\n%s", row, logs)
	}
}

// lastCall is the index of rt's last call starting with prefix (-1:
// none); read once the session's goroutines are done.
func lastCall(rt *sandbox.Fake, prefix string) int {
	idx := -1
	for i, c := range rt.Calls {
		if strings.HasPrefix(c, prefix) {
			idx = i
		}
	}
	return idx
}

// A sandboxed session whose item is no longer this node's — given to
// another node while none ran — is not finished: its container is
// stopped, its pod and HOME removed, its session dir gone, and nothing
// is handed in.
func TestAdoptedSandboxedSessionNoLongerOursIsStoppedAndRemoved(t *testing.T) {
	f, cfg, rt, sb, _, root, origin, s := startSandboxedBuild(t)
	f.SetItem(item1, map[string]any{"status": "running", "nodeId": "99999999-9999-4999-8999-999999999999"})

	loop2, _ := sandboxNode(t, f, cfg, root, origin, sb, &syncBuf{})
	found, err := FindSessionsIn(root, []string{cfg.NodeID}, sb)
	if err != nil || len(found) != 1 {
		t.Fatalf("found %+v, %v", found, err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	loop2.AdoptSessions(ctx, found)
	loop2.wg.Wait()

	if !slices.Contains(rt.StoppedAgents(), s.State.Container) {
		t.Fatalf("container %s not stopped: %v", s.State.Container, rt.Calls)
	}
	if !slices.Contains(rt.Removed, s.State.Pod) {
		t.Fatalf("pod %s not removed: %v", s.State.Pod, rt.Calls)
	}
	if _, err := os.Stat(s.State.Home); !os.IsNotExist(err) {
		t.Fatalf("HOME %s left: %v", s.State.Home, err)
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatalf("session dir left: %v", err)
	}
	if n := len(f.CallsNamed("finishWorkItem")) + len(f.CallsNamed("failWorkItem")); n != 0 {
		t.Fatalf("handed in %d times for an item not ours", n)
	}
}

// The start-up sweep keeps a found sandboxed session's pod, its exited
// agent's container and its HOME — the session ended while no node ran,
// and its adopter still reads and validates it — and sweeps the rest.
func TestStartUpSweepKeepsAFoundSandboxedSession(t *testing.T) {
	tmp := t.TempDir()
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, TempDir: tmp}
	kept, err := sb.Open("ship-it/1", "build", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	// the process before this one left both; this one has none open yet
	kept.Detach()
	if err := kept.Close(); err != nil {
		t.Fatal(err)
	}
	stale := leftHome(t, tmp)
	rt.Running = []sandbox.Container{
		{Name: "mesh-kept-0-agent", Pod: "mesh-kept-0"},
		{Name: "mesh-stale-0-agent", Pod: "mesh-stale-0"},
	}
	rt.Standing = []sandbox.PodInfo{{Name: "mesh-kept-0"}, {Name: "mesh-stale-0"}}
	found := []FoundSession{{Sandbox: sb, State: sessionunit.State{Kind: "build", Runner: sessionunit.SandboxRunner,
		Pod: "mesh-kept-0", Container: "mesh-kept-0-agent", Home: kept.Home}}}
	var lines []string
	SweepSandbox(context.Background(), sb, found, func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })

	if !slices.Equal(rt.Removed, []string{"mesh-stale-0"}) || !slices.Equal(rt.RemovedContainers, []string{"mesh-stale-0-agent"}) {
		t.Fatalf("removed pods %v, containers %v", rt.Removed, rt.RemovedContainers)
	}
	if _, err := os.Stat(kept.Home); err != nil {
		t.Fatalf("the found session's HOME went: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the stale HOME is left: %v", err)
	}
	if want := "sandbox: swept 1 homes, 1 pods, 1 containers at start (1 homes in use)"; !slices.Contains(lines, want) {
		t.Fatalf("log %q, want %q", lines, want)
	}
}

// Every kind of sandboxed session is found by its state and read by its
// container: alive while it runs, stopped by Kill, and Remove takes its
// pod, HOME and session dir — an evaluation, plan, research or rebase
// session as a build.
func TestFoundSandboxedSessionsOfEveryKind(t *testing.T) {
	for _, kind := range []harness.Kind{harness.KindEvaluate, harness.KindPlan, harness.KindResearch, harness.KindRebase} {
		t.Run(string(kind), func(t *testing.T) {
			rt := &sandbox.Fake{}
			sb := &sandbox.Sandbox{Runtime: rt, TempDir: t.TempDir()}
			sess, err := sb.Open("ship-it/1", string(kind), sandbox.GitIdentity{})
			if err != nil {
				t.Fatal(err)
			}
			pod := sandbox.Pod{Name: "mesh-s-0"}
			if err := rt.StartAgent(context.Background(), pod, sandbox.AgentSpec{Name: sandbox.AgentName(pod)}); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			work := filepath.Join(root, "ship-it", "1")
			if err := os.MkdirAll(sessionunit.Dir(work), 0o755); err != nil {
				t.Fatal(err)
			}
			st := sessionunit.State{NodeID: "n1", ItemID: item1, GoalID: goalID, DirectionID: "d1", Kind: string(kind), Runner: sessionunit.SandboxRunner,
				Unit: sandbox.AgentName(pod), Pod: pod.Name, Container: sandbox.AgentName(pod), Home: sess.Home, Workspace: work, StartedAt: time.Now()}
			if err := sessionunit.WriteState(sessionunit.Dir(work), st); err != nil {
				t.Fatal(err)
			}
			found, err := FindSessionsIn(root, []string{"n1"}, sb)
			if err != nil || len(found) != 1 || !found[0].Sandboxed() || !found[0].Alive() {
				t.Fatalf("found %+v, %v", found, err)
			}
			if _, err := found[0].Kill(); err != nil || !slices.Contains(rt.StoppedAgents(), st.Container) || found[0].Alive() {
				t.Fatalf("kill: %v, stopped %v", err, rt.StoppedAgents())
			}
			if err := found[0].Remove(); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(rt.Removed, pod.Name) {
				t.Fatalf("pod not removed: %v", rt.Calls)
			}
			for _, p := range []string{sess.Home, sessionunit.Dir(work)} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("%s left: %v", p, err)
				}
			}
		})
	}
}
