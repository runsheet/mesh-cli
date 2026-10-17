package node

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// configLock is git refusing to rewrite the remote of a repo cache whose
// .git/config.lock a dead process left behind, as the dl360 node logged
// it on 2026-10-13 for every item of latchkey-mints-a-session-id-token.
const configLock = "git remote set-url origin https://github.com/acme/widgets.git: exit status 128: error: could not lock config file /home/mesh/.mesh/repos/acme/widgets/.git/config: File exists"

// lockedWS is a Workspacer whose every Authorize answers configLock.
type lockedWS struct{ *fakeWS }

func (w *lockedWS) Authorize(context.Context, WorkspaceSpec, *gateway.GitHubToken) error {
	return errors.New(configLock)
}

// releaseRecorder records every ReleaseWorkItem and FailWorkItem the
// session sends, refused or not — the fake keeps only what it applied.
type releaseRecorder struct {
	Gateway
	mu       sync.Mutex
	releases []string
	fails    []string
}

func (g *releaseRecorder) ReleaseWorkItem(ctx context.Context, itemID, nodeID, reason string) error {
	g.mu.Lock()
	g.releases = append(g.releases, reason)
	g.mu.Unlock()
	return g.Gateway.ReleaseWorkItem(ctx, itemID, nodeID, reason)
}

func (g *releaseRecorder) FailWorkItem(ctx context.Context, itemID, reason string) error {
	g.mu.Lock()
	g.fails = append(g.fails, reason)
	g.mu.Unlock()
	return g.Gateway.FailWorkItem(ctx, itemID, reason)
}

// infraReleaseOK asserts the one release is an infrastructure one naming
// git's words, and nothing was failed.
func infraReleaseOK(t *testing.T, rec *releaseRecorder, f *gatewaytest.Fake) {
	t.Helper()
	if len(rec.releases) != 1 || !strings.HasPrefix(rec.releases[0], "infrastructure: ") || !strings.Contains(rec.releases[0], "could not lock config file") {
		t.Fatalf("releases = %q", rec.releases)
	}
	if len(rec.fails) != 0 || len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatalf("FailWorkItem called: %q", rec.fails)
	}
}

// A build whose token cannot be written into the repo cache is released
// as an infrastructure hold — ready again, attempts unchanged, the
// reason its last note, never failed — held off this node, and the
// holding report the next tick sends names it.
func TestABuildWhoseRepoCacheIsLockedIsReleasedAsInfrastructure(t *testing.T) {
	f := seed(t)
	rec := &releaseRecorder{}
	h := &fakeHarness{}
	r, fws, item, row := runnerOnFake(t, f, h, func(g Gateway) Gateway { rec.Gateway = g; return rec })
	f.WorkItems[item1]["attempts"] = 2
	r.Declines = &Declines{}
	r.WS = &lockedWS{fakeWS: fws}
	var buf bytes.Buffer
	r.Log = log.New(&buf, "", 0)

	err := r.RunItem(context.Background(), item)
	if err == nil || !strings.Contains(err.Error(), "could not lock config file") {
		t.Fatalf("err = %v", err)
	}
	if len(h.jobs) != 0 {
		t.Fatalf("a session ran: %d", len(h.jobs))
	}
	infraReleaseOK(t, rec, f)
	it := row(item1)
	if it["status"] != "ready" || it["nodeId"] != nil || asInt(it["attempts"]) != 2 {
		t.Fatalf("row: %v", it)
	}
	if note, _ := it["lastNote"].(string); !strings.HasPrefix(note, "infrastructure: github token: ") || !strings.Contains(note, configLock) {
		t.Fatalf("lastNote %q", note)
	}
	cache := (WorkspaceSpec{Root: fws.root, GoalSlug: "ship-it", Seq: 1}).Dir()
	if n := linesWith(buf.String(), "item ship-it/1: repo cache "+cache, "released as an infrastructure hold, no attempt charged", configLock); n != 1 {
		t.Fatalf("%d release lines naming the repo cache, want 1:\n%s", n, buf.String())
	}
	if !r.Declines.Holds(item1, nil, time.Now().Add(WorkspaceReleaseHold-time.Minute)) {
		t.Fatal("the hold must stand for most of ten minutes")
	}

	// the next tick, sharing the Runner's Declines, claims nothing and
	// its holding report names the hold
	gw := gateway.New(f.URL(), "rs_test")
	loop := &Loop{GW: gw, Cfg: r.Cfg, Scopes: scopesOf(r.Cfg), Interval: time.Hour, Declines: r.Declines, Log: r.Log,
		Dispatch: func(context.Context, gateway.WorkItem) { t.Error("claimed while held") }}
	claims := len(f.CallsNamed("claimWorkItem"))
	loop.Tick(context.Background())
	loop.wg.Wait()
	if len(f.CallsNamed("claimWorkItem")) != claims {
		t.Fatal("claimed again while held")
	}
	hold := loop.Holding()
	if hold == nil {
		t.Fatal("no holding report")
	}
	var found *gateway.HoldingUnclaimed
	for i, u := range hold.Unclaimed {
		if u.GoalID == goalID && u.Kind == "item" {
			found = &hold.Unclaimed[i]
		}
	}
	if found == nil || !strings.HasPrefix(found.Reason, "infrastructure: ") || !strings.Contains(found.Reason, "could not lock config file") || found.Count != 1 {
		t.Fatalf("unclaimed = %+v", hold.Unclaimed)
	}
	if found.Until.Before(time.Now().Add(WorkspaceReleaseHold-time.Minute)) || found.Until.After(time.Now().Add(WorkspaceReleaseHold)) {
		t.Fatalf("until %s, want the hold's end", found.Until)
	}
}

// A release the platform refuses still fails the build, with the same
// reason (ADR-024).
func TestAnInfrastructureReleaseRefusedFailsTheBuild(t *testing.T) {
	f := seed(t)
	f.RefuseReleases = true
	r, fws, item, row := runnerOnFake(t, f, &fakeHarness{}, nil)
	r.WS = &lockedWS{fakeWS: fws}
	r.Log = log.New(&bytes.Buffer{}, "", 0)
	_ = r.RunItem(context.Background(), item)
	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 || !strings.HasPrefix(fails[0].Input["error"].(string), "infrastructure: ") || row(item1)["status"] != "failed" {
		t.Fatalf("fails = %+v", fails)
	}
}

// An evaluation whose token cannot be written into its checkout asks
// for the release with the infrastructure reason, fails nothing, records
// no verdict, leaves the item in evaluating with its attempts, and holds
// it off this node.
func TestAnEvaluationWhoseRepoCacheIsLockedIsReleasedAsInfrastructure(t *testing.T) {
	f, ev, h, _ := secondFixture(t, "normal", "VERDICT: PASS", &fakeCodexH{})
	f.WorkItems[item1]["attempts"] = 2
	rec := &releaseRecorder{Gateway: ev.GW}
	ev.GW = rec
	ev.WS = &lockedWS{fakeWS: ev.WS.(*fakeWS)}
	ev.Declines = &Declines{}
	item, err := rec.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err == nil || !strings.Contains(err.Error(), "could not lock config file") {
		t.Fatalf("err = %v", err)
	}
	if len(h.jobs) != 0 || len(f.CallsNamed("evaluateWorkItem")) != 0 {
		t.Fatalf("an evaluation ran or recorded a verdict: %d jobs", len(h.jobs))
	}
	infraReleaseOK(t, rec, f)
	if it := f.Item(item1); it["status"] != "evaluating" || asInt(it["attempts"]) != 2 {
		t.Fatalf("row: %v", it)
	}
	if why, ok := ev.Declines.Held(item1, nil, time.Now()); !ok || !strings.HasPrefix(why, "infrastructure: ") {
		t.Fatalf("held %v: %q", ok, why)
	}
}

// A rebase whose token cannot be written into the repo cache asks for
// the release with the infrastructure reason, never FailWorkItem, and
// leaves the item's attempts as they were.
func TestARebaseWhoseRepoCacheIsLockedIsReleasedAsInfrastructure(t *testing.T) {
	f := seed(t)
	requestRebase(f, item1, "mesh/ship-it-1")
	f.WorkItems[item1]["attempts"] = 2
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{}
	var logs strings.Builder
	loop, _ := rebaseNodeOn(t, f, "test-node", h, &lockedWS{fakeWS: ws}, ws.root, &logs)
	// the rebaser the loop runs, on a recording gateway and the loop's
	// own Declines
	rec := &releaseRecorder{Gateway: loop.GW}
	loop.Declines = &Declines{}
	rb := &Rebaser{Credentials: testCreds(rec), GW: rec, Harness: h, WS: &lockedWS{fakeWS: ws}, Scopes: loop.Scopes, WorkRoot: ws.root, ProgressEvery: time.Hour, Declines: loop.Declines, Log: log.New(&logs, "", 0)}
	loop.Rebase = func(ctx context.Context, item gateway.WorkItem) { _ = rb.Rebase(ctx, item) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()

	if len(h.jobs) != 0 {
		t.Fatalf("a session ran: %d", len(h.jobs))
	}
	infraReleaseOK(t, rec, f)
	if it := f.Item(item1); asInt(it["attempts"]) != 2 {
		t.Fatalf("row: %v", it)
	}
	if !loop.Declines.Holds(item1, nil, time.Now()) {
		t.Fatal("the rebase must be held off this node")
	}
}

// A rebase whose own worktree cannot be made (ADR-028, A rebase gets its
// own worktree) runs no session and is released as an infrastructure
// hold — attempts unchanged, held off this node — never failed: no
// FailWorkItemRebase, no FailWorkItem, and the token never written into
// anything (ADR-024).
func TestARebaseWhoseWorktreeCannotBeMadeIsReleasedAsInfrastructure(t *testing.T) {
	f := seed(t)
	requestRebase(f, item1, "mesh/ship-it-1")
	f.WorkItems[item1]["attempts"] = 2
	ws := &fakeWS{root: t.TempDir(), rebaseErr: errors.New("git worktree add --quiet --detach /home/mesh/.mesh/work/ship-it/1-rebase refs/remotes/origin/mesh/ship-it-1: exit status 128: error: could not lock config file /home/mesh/.mesh/repos/acme/widgets/.git/config: File exists")}
	h := &fakeHarness{}
	var logs strings.Builder
	loop, _ := rebaseNodeOn(t, f, "test-node", h, ws, ws.root, &logs)
	rec := &releaseRecorder{Gateway: loop.GW}
	loop.Declines = &Declines{}
	rb := &Rebaser{Credentials: testCreds(rec), GW: rec, Harness: h, WS: ws, Scopes: loop.Scopes, WorkRoot: ws.root, ProgressEvery: time.Hour, Declines: loop.Declines, Log: log.New(&logs, "", 0)}
	loop.Rebase = func(ctx context.Context, item gateway.WorkItem) { _ = rb.Rebase(ctx, item) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()

	if len(h.jobs) != 0 {
		t.Fatalf("a session ran: %d", len(h.jobs))
	}
	infraReleaseOK(t, rec, f)
	if !strings.HasPrefix(rec.releases[0], "infrastructure: workspace: git worktree add") {
		t.Fatalf("release reason = %q", rec.releases[0])
	}
	if n := len(f.CallsNamed("failWorkItemRebase")); n != 0 {
		t.Fatalf("failWorkItemRebase called %d time(s)\n%s", n, logs.String())
	}
	if len(ws.authorized) != 0 || len(ws.prepared) != 0 {
		t.Fatalf("the token went into a checkout: authorized %v, prepared %v", ws.authorized, ws.prepared)
	}
	if it := f.Item(item1); asInt(it["attempts"]) != 2 {
		t.Fatalf("row: %v", it)
	}
	if !loop.Declines.Holds(item1, nil, time.Now()) {
		t.Fatal("the rebase must be held off this node")
	}
	if ev := strings.Join(ws.Events(), ","); ev != "rebase-worktree mesh/ship-it-1,remove 1-rebase" {
		t.Fatalf("workspace events: %s", ev)
	}
}

// A token the platform refuses is not the node's fault: no
// *WorkspaceError, and the build is failed as before.
func TestATokenRefusalIsNoWorkspaceError(t *testing.T) {
	f := seed(t)
	r, fws, item, _ := runnerOnFake(t, f, &fakeHarness{}, func(g Gateway) Gateway { return refusingTokenGW{g} })
	r.WS = &lockedWS{fakeWS: fws}
	r.Log = log.New(&bytes.Buffer{}, "", 0)
	_, err := NewItemCred(context.Background(), r.GW, r.WS, r.Scopes, item, WorkspaceSpec{Root: fws.root, GoalSlug: "ship-it", Seq: 1, Repo: "acme/widgets"}, nil, r.logf)
	if err == nil || WorkspaceErrorOf(err) != nil || !strings.HasPrefix(err.Error(), "github token: ") {
		t.Fatalf("err = %v (%T)", err, err)
	}
	_ = r.RunItem(context.Background(), item)
	if len(f.CallsNamed("releaseWorkItem")) != 0 || len(f.CallsNamed("failWorkItem")) != 1 {
		t.Fatalf("releases %d, fails %d", len(f.CallsNamed("releaseWorkItem")), len(f.CallsNamed("failWorkItem")))
	}
}
