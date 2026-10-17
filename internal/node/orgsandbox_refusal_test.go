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

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// twoScopes are a node serving acme and beta.
var twoScopes = Scopes{{Login: "acme", ID: "org-a", NodeID: "node-a"}, {Login: "beta", ID: "org-b", NodeID: "node-b"}}

// A node serving two orgs with sandbox off refuses a session of either,
// the error naming the org, the checkout and both orgs, kinded
// sandbox_required; with a sandbox it refuses nothing.
func TestHostFallbackRefusesATwoOrgNodeWithSandboxOff(t *testing.T) {
	dir := "/home/mesh/.mesh/orgs/acme/work/ship-it/1"
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

// A checkout under one org's tree confines its pod to that tree; one
// under today's single work root confines nothing.
func TestOrgRootOfACheckout(t *testing.T) {
	mesh := "/home/mesh/.mesh"
	for dir, want := range map[string]sandbox.OrgRoot{
		mesh + "/orgs/acme/work/ship-it/1":               {Org: "acme", Path: mesh + "/orgs/acme"},
		mesh + "/orgs/acme/work/merge-batches/b-1":       {Org: "acme", Path: mesh + "/orgs/acme"},
		mesh + "/orgs/beta/work/ship-it/1-eval":          {Org: "beta", Path: mesh + "/orgs/beta"},
		mesh + "/work/ship-it/1":                         {},
		mesh + "/orgs/acme/repos/o/n":                    {},
		"/elsewhere/orgs/acme/work/x/orgs/beta/work/y/1": {Org: "beta", Path: "/elsewhere/orgs/acme/work/x/orgs/beta"},
	} {
		if got := orgRootOf(dir); got != want {
			t.Errorf("orgRootOf(%s) = %+v, want %+v", dir, got, want)
		}
	}
}
