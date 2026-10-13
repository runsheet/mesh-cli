//go:build !windows

package node

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// Goal a-sandboxed-session-outlives-the-node, item 4: a sandboxed node's
// self-update swaps at once like an unsandboxed one's — its build's agent
// runs detached in its pod, which outlives the node — and the next
// process adopts the session, heartbeats the item inside the platform's
// stall window across the swap, and pushes and finishes it: no drain, no
// stall, no second claim.
func TestSandboxedSelfUpdateMidSessionFinishesOnTheNewNode(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	f := seed(t)
	cfg := registeredNode(t, f)
	rt := &sandbox.Fake{}
	agent := newDetachedAgent(rt)
	t.Cleanup(func() { closeOnce(agent.release) })
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	sb.Config.ServiceTimeout = 2 * time.Second
	root := t.TempDir()
	origin := originRepo(t, map[string]string{".runsheet/validate.yml": validateYML, sandbox.ServicesFile: servicesYML})

	// the old node: sandboxed, on v1.0.0, ticking often enough to read
	// its desired version while the build runs
	logs1 := &syncBuf{}
	loop1, runner1 := sandboxNode(t, f, cfg, root, origin, sb, logs1)
	loop1.Sandbox = &gateway.SandboxInfo{Mode: SandboxOn}
	loop1.Interval = 50 * time.Millisecond
	loop1.Version = "v1.0.0"
	u := &fakeUpdater{loop: loop1}
	loop1.Update = u
	ran := make(chan error, 1)
	go func() { ran <- loop1.Run(context.Background()) }()
	select {
	case <-agent.started:
	case <-time.After(10 * time.Second):
		t.Fatalf("the agent never started: %v\n%s", rt.Calls, logs1)
	}
	var found []FoundSession
	waitFor(t, func() bool {
		found, _ = FindSessionsIn(root, []string{cfg.NodeID}, sb)
		return len(found) == 1 && found[0].State.Offset > 0
	})
	s := found[0]

	// the platform asks for v1.1.0 mid-session: Run returns at once,
	// the agent still running in its pod
	f.SetNode(cfg.NodeID, map[string]any{"desiredVersion": "v1.1.0"})
	select {
	case err := <-ran:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("Run = %v, want ErrRestartForUpdate\n%s", err, logs1)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Run waited for the sandboxed session:\n%s", logs1)
	}
	loop1.wg.Wait()
	swappedAt := time.Now()
	if loop1.Draining() {
		t.Fatal("the sandboxed update drained")
	}
	if got := u.Events(); !slices.Contains(got, "swap mesh v1.1.0 running") {
		t.Fatalf("updater events %q: no swap with the session running", got)
	}
	out1 := logs1.String()
	if !strings.Contains(out1, "self-update: v1.1.0 verified; no drain: swapping and restarting, sessions left running for the new binary to adopt (sandbox: on)") ||
		!strings.Contains(out1, "1 session(s) left for it to adopt") {
		t.Fatalf("the swap does not say the sandboxed session is left for the new binary:\n%s", out1)
	}
	if !s.Alive() || len(rt.StoppedAgents()) != 0 || slices.Contains(rt.Removed, s.State.Pod) {
		t.Fatalf("the agent did not outlive the old node: alive %v, stopped %v, removed %v", s.Alive(), rt.StoppedAgents(), rt.Removed)
	}
	if _, err := os.Stat(s.State.Home); err != nil {
		t.Fatalf("the session's HOME went with the old node: %v", err)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 0 {
		t.Fatalf("finished before the swap: %d", n)
	}
	if !runner1.WS.(*gitWS).pushedAt.IsZero() {
		t.Fatal("the old node pushed")
	}

	// the new binary's process: finds the session, adopts it at once
	logs2 := &syncBuf{}
	loop2, runner2 := sandboxNode(t, f, cfg, root, origin, sb, logs2)
	loop2.Sandbox = &gateway.SandboxInfo{Mode: SandboxOn}
	found, err := FindSessionsIn(root, []string{cfg.NodeID}, sb)
	if err != nil || len(found) != 1 || !found[0].Sandboxed() || !found[0].Alive() || found[0].State.Container != s.State.Container {
		t.Fatalf("found %+v, %v", found, err)
	}
	SweepSandbox(context.Background(), sb, found, func(string, ...any) {})
	ctx2, stop2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop2()
	if n := loop2.AdoptSessions(ctx2, found); n != 1 {
		t.Fatalf("adopted %d\n%s", n, logs2)
	}
	loop2.Tick(ctx2) // capacity 1 is the adopted build's: nothing claimed
	// the agent keeps working on the new node for a while before it ends
	time.Sleep(300 * time.Millisecond)
	closeOnce(agent.release)
	waitFor(t, func() bool { return len(f.CallsNamed("finishWorkItem")) == 1 })
	loop2.wg.Wait()

	if runner2.WS.(*gitWS).pushedAt.IsZero() {
		t.Fatalf("the new node did not push\n%s", logs2)
	}
	if n := len(f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("claimWorkItem calls = %d, want the old node's alone", n)
	}
	if c := f.CallsNamed("finishWorkItem")[0]; c.At.Before(swappedAt) || c.Input["aggregateId"] != item1 {
		t.Fatalf("finish %+v not by the new node", c)
	}
	if row := f.Item(item1); row["status"] != "evaluating" || row["branch"] != "mesh/ship-it-1" || row["nodeId"] != cfg.NodeID {
		t.Fatalf("item row %v\n%s", row, logs2)
	}
	// the item's heartbeats, across the swap included: no gap as long as
	// this test's stall window, and the new node's begin at the adoption
	var gap time.Duration
	var last time.Time
	after := 0
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if c.Input["aggregateId"] != item1 {
			continue
		}
		if !last.IsZero() && c.At.Sub(last) > gap {
			gap = c.At.Sub(last)
		}
		last = c.At
		if c.At.After(swappedAt) {
			after++
		}
	}
	if stall := 2 * time.Second; gap > stall {
		t.Fatalf("heartbeats stopped for %s across the swap (stall window %s)", gap, stall)
	}
	if after < 2 {
		t.Fatalf("%d heartbeats after the swap, want the adoption's and the session's", after)
	}
	if strings.Contains(logs2.String(), "stall") {
		t.Fatalf("the new node logged a stall:\n%s", logs2)
	}
	if !slices.Contains(rt.Removed, s.State.Pod) {
		t.Fatalf("the session's pod %s was not removed after the finish: %v", s.State.Pod, rt.Calls)
	}
	if _, err := os.Stat(s.State.Home); !os.IsNotExist(err) {
		t.Fatalf("the session's HOME %s is left: %v", s.State.Home, err)
	}
}
