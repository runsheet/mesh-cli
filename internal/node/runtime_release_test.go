package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// podmanStorage is podman's own words for a rootless store a root
// `podman` run poisoned, as grapevine's node.log had them on 2026-10-16.
const podmanStorage = `Error: error opening storage: creating temporary storage: open /home/mesh/.local/share/containers/storage/overlay/l: permission denied`

// brokenRuntime is a sandbox Fake whose every CreatePod fails as
// Podman.CreatePod answers a broken store: a *sandbox.RuntimeError.
func brokenRuntime() *sandbox.Fake {
	return &sandbox.Fake{CreatePodErr: &sandbox.RuntimeError{Op: "pod create", Err: errors.New(podmanStorage)}}
}

// hookCount counts the RuntimeBroken hook's calls.
type hookCount struct {
	mu   sync.Mutex
	errs []error
}

func (h *hookCount) hook(err error) {
	h.mu.Lock()
	h.errs = append(h.errs, err)
	h.mu.Unlock()
}

func (h *hookCount) once(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.errs) != 1 {
		t.Fatalf("RuntimeBroken hook called %d times, want 1: %v", len(h.errs), h.errs)
	}
	var re *sandbox.RuntimeError
	if !errors.As(h.errs[0], &re) {
		t.Fatalf("hook told %v, want the *sandbox.RuntimeError", h.errs[0])
	}
}

// evalRecorder records the evaluator's verdicts and no-verdicts.
type evalRecorder struct {
	Gateway
	mu         sync.Mutex
	verdicts   int
	noVerdicts []string
}

func (g *evalRecorder) EvaluateWorkItem(ctx context.Context, itemID string, passed bool, notes string, clauses []gateway.FailedClause, costCents int, usage *gateway.UsageReport, choice gateway.EvaluateChoice) error {
	g.mu.Lock()
	g.verdicts++
	g.mu.Unlock()
	return g.Gateway.EvaluateWorkItem(ctx, itemID, passed, notes, clauses, costCents, usage, choice)
}

func (g *evalRecorder) EvaluateWorkItemNoVerdict(ctx context.Context, itemID, reason string, costCents int, usage *gateway.UsageReport, choice gateway.EvaluateChoice) error {
	g.mu.Lock()
	g.noVerdicts = append(g.noVerdicts, reason)
	g.mu.Unlock()
	return g.Gateway.EvaluateWorkItemNoVerdict(ctx, itemID, reason, costCents, usage, choice)
}

// runtimeReleaseOK asserts the one release is runtime_broken naming
// podman's words, and nothing was failed.
func runtimeReleaseOK(t *testing.T, rec *releaseRecorder, f *gatewaytest.Fake) {
	t.Helper()
	if len(rec.releases) != 1 || !strings.HasPrefix(rec.releases[0], "runtime_broken: ") || !strings.Contains(rec.releases[0], "error opening storage") {
		t.Fatalf("releases = %q", rec.releases)
	}
	if len(rec.fails) != 0 || len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatalf("FailWorkItem called: %q", rec.fails)
	}
}

// A build whose pod the broken runtime cannot start — the harness's own
// pod, or the services' pod the node opens first — is released
// runtime_broken, attempts unchanged, the reason its last note, never
// failed; held off this node, the hook told once, one log line naming
// the item and podman's words.
func TestRuntimeReleaseOfABuild(t *testing.T) {
	for _, tc := range []struct{ name, yml string }{{"harness pod", ""}, {"services pod", servicesYML}} {
		t.Run(tc.name, func(t *testing.T) {
			rt := brokenRuntime()
			sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
			f := seed(t)
			rec := &releaseRecorder{}
			r, ws, item, row := runnerOnFake(t, f, &harness.ClaudeCode{Sandbox: sb}, func(g Gateway) Gateway { rec.Gateway = g; return rec })
			f.WorkItems[item1]["attempts"] = 2
			r.Sandbox, r.Declines = sb, &Declines{}
			writeServices(t, filepath.Join(ws.root, "ship-it", "1"), tc.yml)
			seen := fakeAgent(rt, nil)
			var hooks hookCount
			r.RuntimeBroken = hooks.hook
			var buf bytes.Buffer
			r.Log = log.New(&buf, "", 0)

			err := r.RunItem(context.Background(), item)
			if err == nil || !strings.Contains(err.Error(), "error opening storage") || seen.ran {
				t.Fatalf("err = %v ran=%v", err, seen.ran)
			}
			runtimeReleaseOK(t, rec, f)
			it := row(item1)
			if it["status"] != "ready" || asInt(it["attempts"]) != 2 {
				t.Fatalf("row: %v", it)
			}
			if note, _ := it["lastNote"].(string); !strings.HasPrefix(note, "runtime_broken: pod create: ") {
				t.Fatalf("lastNote %q", note)
			}
			if n := linesWith(buf.String(), "item ship-it/1: container runtime broken, released, no attempt charged", "error opening storage"); n != 1 {
				t.Fatalf("%d release lines, want 1:\n%s", n, buf.String())
			}
			if !r.Declines.Holds(item1, nil, time.Now().Add(WorkspaceReleaseHold-time.Minute)) {
				t.Fatal("the hold must stand for most of ten minutes")
			}
			hooks.once(t)
		})
	}
}

// An evaluation whose pod the broken runtime cannot start is recorded
// as no verdict with the runtime_broken reason — never a verdict — so
// the item's notes read an environment failure; the hook is told once.
func TestRuntimeReleaseOfAnEvaluation(t *testing.T) {
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"
	rt := brokenRuntime()
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	f := seed(t)
	f.WorkItems[item1]["status"] = "evaluating"
	ws := &fakeWS{root: t.TempDir()}
	seen := fakeAgent(rt, nil)
	gw := gateway.New(f.URL(), "rs_test")
	rec := &evalRecorder{Gateway: gw}
	var hooks hookCount
	var buf bytes.Buffer
	ev := &Evaluator{Credentials: testCreds(gw), GW: rec, Harness: &harness.ClaudeCode{Sandbox: sb}, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Sandbox: sb, Declines: &Declines{}, RuntimeBroken: hooks.hook, Log: log.New(&buf, "", 0)}
	if err := ev.Evaluate(context.Background(), item); err != nil || seen.ran {
		t.Fatalf("evaluate: %v ran=%v\n%s", err, seen.ran, buf.String())
	}
	if rec.verdicts != 0 {
		t.Fatalf("EvaluateWorkItem called %d times", rec.verdicts)
	}
	if len(rec.noVerdicts) != 1 || !strings.HasPrefix(rec.noVerdicts[0], "runtime_broken: ") || !strings.Contains(rec.noVerdicts[0], "error opening storage") {
		t.Fatalf("no verdicts = %q", rec.noVerdicts)
	}
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 1 || calls[0].Input[gateway.NoVerdictField] != true {
		t.Fatalf("evaluateWorkItem = %+v", calls)
	}
	if n := linesWith(buf.String(), "item ship-it/1: container runtime broken, not evaluated", "error opening storage"); n != 1 {
		t.Fatalf("%d runtime lines, want 1:\n%s", n, buf.String())
	}
	hooks.once(t)
}

// A platform without the no-verdict outcome leaves the evaluation in
// evaluating, logged, as before; the hook is still told.
func TestRuntimeReleaseOfAnEvaluationWithoutNoVerdict(t *testing.T) {
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"
	rt := brokenRuntime()
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	f := seed(t)
	f.NoNoVerdict = true
	f.WorkItems[item1]["status"] = "evaluating"
	ws := &fakeWS{root: t.TempDir()}
	writeServices(t, EvalWorkspaceSpec(ws.root, "ship-it", 1, "", "", "").Dir(), servicesYML)
	gw := gateway.New(f.URL(), "rs_test")
	var hooks hookCount
	var buf bytes.Buffer
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: &harness.ClaudeCode{Sandbox: sb}, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Sandbox: sb, RuntimeBroken: hooks.hook, Log: log.New(&buf, "", 0)}
	err := ev.Evaluate(context.Background(), item)
	if err == nil || !strings.Contains(err.Error(), "runtime_broken: ") {
		t.Fatalf("evaluate: %v", err)
	}
	if f.Item(item1)["status"] != "evaluating" || !strings.Contains(buf.String(), "left in evaluating: runtime_broken: ") {
		t.Fatalf("status %v\n%s", f.Item(item1)["status"], buf.String())
	}
	hooks.once(t)
}

// A plan whose session's pod the broken runtime cannot start releases
// its plan claim with the runtime_broken reason, counts no try, and
// tells the hook once.
func TestRuntimeReleaseOfAPlan(t *testing.T) {
	f := widgetsAlone(t)
	var logs bytes.Buffer
	pl, _, _, cfg := planTokenPlanner(f, t, &logs)
	rt := brokenRuntime()
	pl.Harness = &harness.ClaudeCode{Sandbox: &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}}
	seen := fakeAgent(rt, nil)
	var hooks hookCount
	pl.RuntimeBroken = hooks.hook
	if err := pl.Plan(context.Background(), shipIt(), scopesOf(cfg)[0]); err == nil || seen.ran {
		t.Fatalf("plan: %v ran=%v", err, seen.ran)
	}
	calls := f.CallsNamed("releasePlan")
	if len(calls) != 1 {
		t.Fatalf("releasePlan calls = %+v\n%s", calls, logs.String())
	}
	if note, _ := calls[0].Input["note"].(string); !strings.HasPrefix(note, "runtime_broken: ") || !strings.Contains(note, "error opening storage") {
		t.Fatalf("note %q", note)
	}
	if n := linesWith(logs.String(), "goal ship-it: container runtime broken; plan claim released", "error opening storage"); n != 1 {
		t.Fatalf("%d release lines, want 1:\n%s", n, logs.String())
	}
	if pl.attempts[goalID] != 0 {
		t.Fatalf("attempts = %d, want 0", pl.attempts[goalID])
	}
	hooks.once(t)
}

// A rebase whose services' pod the broken runtime cannot start is
// released runtime_broken (refused here, its failure carries the same
// reason, never "services: …"); the hook is told.
func TestRuntimeReleaseOfARebase(t *testing.T) {
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"
	rt := brokenRuntime()
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	f := seed(t)
	f.WorkItems[item1]["status"] = "review"
	f.WorkItems[item1]["rebaseState"] = "claimed"
	f.RebaseTokens = true
	ws := &fakeWS{root: t.TempDir()}
	writeServices(t, filepath.Join(ws.root, "ship-it", "1"), servicesYML)
	seen := fakeAgent(rt, nil)
	gw := gateway.New(f.URL(), "rs_test")
	rec := &releaseRecorder{Gateway: gw}
	var hooks hookCount
	rb := &Rebaser{Credentials: testCreds(gw), GW: rec, Harness: &harness.ClaudeCode{Sandbox: sb}, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Sandbox: sb, RuntimeBroken: hooks.hook, Log: log.New(&bytes.Buffer{}, "", 0)}
	if err := rb.Rebase(context.Background(), item); err == nil || seen.ran {
		t.Fatalf("rebase: %v ran=%v", err, seen.ran)
	}
	if len(rec.releases) != 1 || !strings.HasPrefix(rec.releases[0], "runtime_broken: ") {
		t.Fatalf("releases = %q", rec.releases)
	}
	// the platform takes no release of an item in review: refused, the
	// rebase is failed with the same runtime_broken reason, as an
	// infrastructure release refused is
	if fails := f.CallsNamed("failWorkItemRebase"); len(fails) != 1 || !strings.HasPrefix(fmt.Sprint(fails[0].Input["reason"]), "runtime_broken: ") {
		t.Fatalf("rebase fails: %+v", fails)
	}
	hooks.once(t)
}
