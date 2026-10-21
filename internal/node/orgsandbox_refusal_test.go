package node

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// twoScopes are a node serving acme and beta.
var twoScopes = Scopes{{Login: "acme", ID: "org-a", NodeID: "node-a"}, {Login: "beta", ID: "org-b", NodeID: "node-b"}}

// A node serving two orgs with sandbox off refuses a session of either,
// the error naming the org, the checkout and both orgs, kinded
// sandbox_required; with a sandbox it refuses nothing.
func TestHostFallbackRefusesATwoOrgNodeWithSandboxOff(t *testing.T) {
	dir := "/home/mesh/.mesh/jobs/item-1/checkout"
	err := HostFallback(nil, twoScopes, "org-a", dir)
	if err == nil {
		t.Fatal("a two-org node with sandbox off was not refused")
	}
	if !errors.Is(err, ErrHostServesSeveralOrgs) || validate.Kinded(err) != validate.SandboxRequired {
		t.Fatalf("%v: not a sandbox_required refusal", err)
	}
	for _, s := range []string{"acme", "beta", dir} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("%q does not name %q", err, s)
		}
	}
	if got := HostFallbackReason(err); !strings.HasPrefix(got, SandboxRequiredPrefix) || !strings.Contains(got, dir) {
		t.Fatalf("reason %q", got)
	}
	sb := &sandbox.Sandbox{Runtime: &sandbox.Fake{}}
	if err := HostFallback(sb, twoScopes, "org-a", dir); err != nil {
		t.Fatalf("a sandboxed two-org node refused: %v", err)
	}
}

// A node serving one org runs its sessions on the host as before.
func TestHostFallbackAllowsAOneOrgNode(t *testing.T) {
	if err := HostFallback(nil, twoScopes[:1], "org-a", "/home/mesh/.mesh/work/ship-it/1"); err != nil {
		t.Fatalf("a one-org node refused: %v", err)
	}
}

// A build on a two-org node with sandbox off is released sandbox_required
// before any checkout or session, named for its org and checkout, never
// failed, held off this node; the refusal is said once in node.log.
func TestRunItemOnATwoOrgNodeWithSandboxOffIsReleased(t *testing.T) {
	hostFallbackSaid = sync.Once{}
	t.Cleanup(func() { hostFallbackSaid = sync.Once{} })
	f := seed(t)
	rec := &releaseRecorder{}
	h := &fakeHarness{}
	r, _, item, row := runnerOnFake(t, f, h, func(g Gateway) Gateway { rec.Gateway = g; return rec })
	r.Scopes = append(r.Scopes, Scope{Login: "beta", ID: "org-b", NodeID: "node-b"})
	r.Sandbox, r.Declines = nil, &Declines{}
	var buf bytes.Buffer
	r.Log = log.New(&buf, "", 0)

	err := r.RunItem(context.Background(), item)
	if !errors.Is(err, ErrHostServesSeveralOrgs) {
		t.Fatalf("err = %v", err)
	}
	if len(h.jobs) != 0 {
		t.Fatal("a session ran on the host")
	}
	dir := filepath.Join(r.WorkRoot, "ship-it", "1")
	if len(rec.releases) != 1 || !strings.HasPrefix(rec.releases[0], SandboxRequiredPrefix) || !strings.Contains(rec.releases[0], "acme") || !strings.Contains(rec.releases[0], dir) {
		t.Fatalf("releases = %q", rec.releases)
	}
	if len(rec.fails) != 0 {
		t.Fatalf("failed: %q", rec.fails)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("the checkout %s was made", dir)
	}
	if it := row(item1); it["status"] != "ready" {
		t.Fatalf("row: %v", it)
	}
	if !r.Declines.Holds(item1, nil, time.Now()) {
		t.Fatal("not held off this node")
	}
	// a second refusal is not said again
	_ = ReleaseOnHostFallback(context.Background(), rec, item1, "node-a", "ship-it/1", err, r.logf)
	if n := strings.Count(buf.String(), "every session is handed back until the sandbox is on"); n != 1 {
		t.Fatalf("said %d times, want once:\n%s", n, buf.String())
	}
}

// A one-org node with sandbox off still builds on the host.
func TestRunItemOnAOneOrgNodeWithSandboxOffRuns(t *testing.T) {
	f := seed(t)
	h := &fakeHarness{}
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	r.Sandbox = nil
	_ = r.RunItem(context.Background(), item)
	if len(h.jobs) == 0 {
		t.Fatal("a one-org node with sandbox off ran no session")
	}
}

// A checkout in a job confines its pod to the job's directory alone
// (ADR-039); a directory in no job — a job directory with no job.json,
// anything outside the jobs tree — confines nothing.
func TestConfinementOfACheckout(t *testing.T) {
	l := testLayout(t)
	scopes := connected(Scopes{{Login: "acme", ID: "org-a"}})
	j, err := openJob(l, scopes, "org-a", JobMeta{ID: "item-1", Unit: JobItem, ItemID: "item-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := sandbox.Confinement{Job: "item-1", Roots: []string{j.Dir}, Homes: j.Homes}
	for _, dir := range []string{j.Checkout(), j.Eval(), j.Attempt("candidate", 2)} {
		got := confinementOf(dir)
		if got.Job != want.Job || got.Homes != want.Homes || len(got.Roots) != 1 || got.Roots[0] != want.Roots[0] {
			t.Errorf("confinementOf(%s) = %+v, want %+v", dir, got, want)
		}
	}
	bare := filepath.Join(l.JobsDir(), "no-job-json", "checkout")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{bare, t.TempDir()} {
		if got := confinementOf(dir); !got.IsZero() {
			t.Errorf("confinementOf(%s) = %+v, want none", dir, got)
		}
	}
}

// Every kind of session is refused on the host of a node serving two
// orgs with no sandbox (ADR 066, decision 10), not only builds and
// rebases: the pod an evaluation or a main validation opens
// (openOrgSessionPod) and the one a plan or research opens
// (openScopePod, on the orgs the loop serves) are refused alike, and
// both open nothing on a one-org node.
func TestEverySessionKindIsRefusedOnATwoOrgHost(t *testing.T) {
	ctx := context.Background()
	dir := "/home/mesh/.mesh/work/ship-it/1"
	logf := func(string, ...any) {}
	pod, err := openOrgSessionPod(ctx, nil, nil, twoScopes, "org-a", dir, "e-1", harness.KindEvaluate, sandbox.GitIdentity{}, logf)
	if pod != nil || !errors.Is(err, ErrHostServesSeveralOrgs) {
		t.Fatalf("an evaluation's pod on a two-org host: %v %v", pod, err)
	}
	pod, err = openScopePod(ctx, nil, nil, twoScopes, twoScopes[0], dir, "p-1", harness.KindPlan, logf)
	if pod != nil || !errors.Is(err, ErrHostServesSeveralOrgs) {
		t.Fatalf("a plan's pod on a two-org host: %v %v", pod, err)
	}
	if pod, err := openOrgSessionPod(ctx, nil, nil, twoScopes[:1], "org-a", dir, "e-1", harness.KindEvaluate, sandbox.GitIdentity{}, logf); pod != nil || err != nil {
		t.Fatalf("an evaluation on a one-org host: %v %v", pod, err)
	}
	if pod, err := openScopePod(ctx, nil, nil, twoScopes[:1], twoScopes[0], dir, "p-1", harness.KindResearch, logf); pod != nil || err != nil {
		t.Fatalf("research on a one-org host: %v %v", pod, err)
	}
	if pod, err := openScopePod(ctx, nil, nil, nil, twoScopes[0], dir, "p-1", harness.KindPlan, logf); pod != nil || err != nil {
		t.Fatalf("a plan with no served orgs given: %v %v", pod, err)
	}
}
