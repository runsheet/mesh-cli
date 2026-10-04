package node

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// lockRef is git refusing a ref update another process was writing at
// the same moment, as the grapevine node logged it on 2026-10-03.
const lockRef = "git fetch --prune origin: exit status 1: error: cannot lock ref 'refs/remotes/origin/main': is at c67b4444 but expected d843d265 (unable to update local ref)"

type workspaceRun struct {
	err      error
	h        *fakeHarness
	r        *Runner
	row      map[string]any
	attempts int
	logged   string
}

// runWorkspaceFailure runs item 1 through the Runner against f with a
// Workspacer whose every Prepare answers lockRef.
func runWorkspaceFailure(t *testing.T, f *gatewaytest.Fake) workspaceRun {
	t.Helper()
	noSleep(t)
	tg := fourTokens()
	h := &fakeHarness{}
	r, fws, item, row := runnerOnFake(t, f, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
	f.WorkItems[item1]["attempts"] = 2
	r.Declines = &Declines{}
	r.WS = &refusingWS{fakeWS: fws, refusals: 100, err: errors.New(lockRef)}
	var buf bytes.Buffer
	r.Log = log.New(&buf, "", 0)
	err := r.RunItem(context.Background(), item)
	return workspaceRun{err: err, h: h, r: r, row: row(item1), attempts: 2, logged: buf.String()}
}

func linesWith(s string, subs ...string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		all := true
		for _, sub := range subs {
			all = all && strings.Contains(line, sub)
		}
		if all {
			n++
		}
	}
	return n
}

// A workspace that cannot be prepared releases the claim with the
// error — ready again, no node, attempts unchanged, never failed — and
// this node does not claim the item again while the hold stands
// (ADR-024).
func TestAWorkspaceFailureReleasesTheClaim(t *testing.T) {
	f := seed(t)
	run := runWorkspaceFailure(t, f)
	if run.err == nil || !strings.Contains(run.err.Error(), "cannot lock ref") {
		t.Fatalf("err = %v", run.err)
	}
	if len(run.h.jobs) != 0 {
		t.Fatalf("a session ran: %d", len(run.h.jobs))
	}
	if n := len(f.CallsNamed("failWorkItem")); n != 0 {
		t.Fatalf("FailWorkItem called %d times", n)
	}
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 || rel[0].Input["aggregateId"] != item1 {
		t.Fatalf("releases = %+v", rel)
	}
	if run.row["status"] != "ready" || run.row["nodeId"] != nil || asInt(run.row["attempts"]) != run.attempts {
		t.Fatalf("row: %v", run.row)
	}
	note, _ := run.row["lastNote"].(string)
	if !strings.HasPrefix(note, "workspace: ") || !strings.Contains(note, "cannot lock ref") || !strings.Contains(note, lockRef) {
		t.Fatalf("lastNote %q", note)
	}
	if n := linesWith(run.logged, "ship-it/1", "workspace failed, released for a retry", lockRef); n != 1 {
		t.Fatalf("%d release lines naming the item and the error, want 1:\n%s", n, run.logged)
	}
	if n := linesWith(run.logged, "item ship-it/1: held off this node until "); n != 1 {
		t.Fatalf("%d hold lines, want 1:\n%s", n, run.logged)
	}
	if !run.r.Declines.Holds(item1, nil, time.Now().Add(WorkspaceReleaseHold-time.Minute)) {
		t.Fatal("the hold must stand for most of ten minutes")
	}

	// the next tick on the same node, sharing the Runner's Declines as
	// nodecmd wires them, does not claim the item
	gw := gateway.New(f.URL(), "rs_test")
	var dispatched atomic.Int32
	loop := &Loop{GW: gw, Cfg: run.r.Cfg, Scopes: scopesOf(run.r.Cfg), Interval: time.Hour, Declines: run.r.Declines, Log: run.r.Log,
		Dispatch: func(context.Context, gateway.WorkItem) { dispatched.Add(1) }}
	claims := len(f.CallsNamed("claimWorkItem"))
	loop.Tick(context.Background())
	loop.wg.Wait()
	if dispatched.Load() != 0 || len(f.CallsNamed("claimWorkItem")) != claims {
		t.Fatalf("claimed again while held: dispatched %d, claims %d → %d", dispatched.Load(), claims, len(f.CallsNamed("claimWorkItem")))
	}

	// the same tick with no hold claims it: the hold is what kept it
	free := &Loop{GW: gw, Cfg: run.r.Cfg, Scopes: scopesOf(run.r.Cfg), Interval: time.Hour, Declines: &Declines{}, Log: run.r.Log,
		Dispatch: func(context.Context, gateway.WorkItem) { dispatched.Add(1) }}
	free.Tick(context.Background())
	free.wg.Wait()
	if dispatched.Load() != 1 {
		t.Fatalf("without the hold the item must be claimed: dispatched %d\n%s", dispatched.Load(), run.logged)
	}
}

// A platform that refuses the release would leave the item held by a
// node that stopped working on it: the item is failed instead, once,
// and the log says the release was refused.
func TestAWorkspaceFailureWhoseReleaseIsRefusedFails(t *testing.T) {
	f := seed(t)
	f.RefuseReleases = true
	run := runWorkspaceFailure(t, f)
	if run.err == nil {
		t.Fatal("want the workspace error")
	}
	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 {
		t.Fatalf("FailWorkItem called %d times, want 1", len(fails))
	}
	if reason, _ := fails[0].Input["error"].(string); reason != "" && !strings.HasPrefix(reason, "workspace: ") {
		t.Fatalf("fail reason %q", reason)
	}
	if n := linesWith(run.logged, "ship-it/1", "release was refused"); n != 1 {
		t.Fatalf("%d refused-release lines, want 1:\n%s", n, run.logged)
	}
}

// The node's own shutdown releases and fails nothing.
func TestAWorkspaceFailureOnShutdownLeavesTheItem(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	err := ReleaseOnWorkspace(ctx, gw, item1, "n", "ship-it/1", errors.New(lockRef), log.New(&buf, "", 0).Printf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n, m := len(f.CallsNamed("releaseWorkItem")), len(f.CallsNamed("failWorkItem")); n != 0 || m != 0 {
		t.Fatalf("released %d, failed %d", n, m)
	}
}
