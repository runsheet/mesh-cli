package node

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

const (
	orgB    = "0f3b8a1e-2c4d-4e6f-8a9b-0c1d2e3f4a5c"
	itemB   = "eeeeeeee-9999-4999-8999-eeeeeeeeeeee"
	nodeA   = "22222222-2222-4222-8222-222222222222"
	nodeB   = "33333333-3333-4333-8333-333333333333"
	machine = "rs_test"
)

// twoOrgs is a node serving acme and beta, each holding the claim of
// one item, its roots under a temp ~/.mesh, its token files kept by tf
// and its builds' bearers opened by sb.
type twoOrgs struct {
	f      *gatewaytest.Fake
	r      *Runner
	tf     *TokenFiles
	roots  *OrgRootSet
	items  map[string]gateway.WorkItem
	nodeOf map[string]string
}

func newTwoOrgs(t *testing.T) *twoOrgs {
	t.Helper()
	f := gatewaytest.New(machine)
	t.Cleanup(f.Close)
	f.WorkItems[item1] = map[string]any{"id": item1, "orgId": orgID, "status": "claimed", "nodeId": nodeA, "attempts": 1}
	f.WorkItems[itemB] = map[string]any{"id": itemB, "orgId": orgB, "status": "claimed", "nodeId": nodeB, "attempts": 3}
	gw := gateway.New(f.URL(), machine)
	scopes := Scopes{{Login: "acme", ID: orgID, NodeID: nodeA}, {Login: "beta", ID: orgB, NodeID: nodeB}}
	roots, err := NewOrgRootSet(t.TempDir(), scopes.Names())
	if err != nil {
		t.Fatal(err)
	}
	tf := &TokenFiles{Token: gw.Bearer, MultiOrg: func() bool { return len(scopes) > 1 }}
	sb := &SessionBearers{GW: gw, Tokens: tf, Audience: MeshMCPURL(f.URL()), Orgs: func() int { return len(scopes) }}
	r := &Runner{GW: gw, Scopes: scopes, Roots: roots, Bearers: sb, Log: log.New(os.Stderr, "", 0)}
	return &twoOrgs{f: f, r: r, tf: tf, roots: roots,
		items:  map[string]gateway.WorkItem{"acme": {ID: item1, OrgID: orgID, Attempts: 1}, "beta": {ID: itemB, OrgID: orgB, Attempts: 3}},
		nodeOf: map[string]string{"acme": nodeA, "beta": nodeB}}
}

// session places one org's build token file as the harness does: the
// node binds its bearer and names its directory, the keeper keeps it.
func (o *twoOrgs) session(t *testing.T, org string) (path string, unbind func(), release func(bool), err error) {
	t.Helper()
	item := o.items[org]
	job := harness.Job{Kind: harness.KindBuild, Meta: harness.SessionMeta{NodeID: o.nodeOf[org], ItemID: item.ID, Attempt: item.Attempts}}
	unbind, err = o.r.bindSessionBearer(context.Background(), item, o.nodeOf[org], &job)
	if err != nil {
		return "", unbind, nil, err
	}
	path = filepath.Join(job.TokenDir, harness.TokenFileName)
	release, err = o.tf.KeepSession(path, job.Kind, job.Meta)
	return path, unbind, release, err
}

func readTok(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// Two sessions of two orgs get two bearers, each its own mesh
// session's id token and never the machine's, each file under its own
// org's MCP root; the open names the item, its claim and its node; at
// the session's end the bearer is cleared and the file and its
// directory removed (ADR-035).
func TestTwoOrgsSessionsReadOnTheirOwnBearers(t *testing.T) {
	o := newTwoOrgs(t)
	paths := map[string]string{}
	toks := map[string]string{}
	ends := map[string]func(){}
	for _, org := range []string{"acme", "beta"} {
		path, unbind, release, err := o.session(t, org)
		if err != nil {
			t.Fatalf("%s: %v", org, err)
		}
		paths[org], toks[org] = path, readTok(t, path)
		ends[org] = func() { unbind(); release(true); RemoveTokenDir(filepath.Dir(path)) }
		roots, _ := o.roots.For(org)
		if !strings.HasPrefix(path, roots.MCP+string(filepath.Separator)) {
			t.Fatalf("%s's token file %s is not under its MCP root %s", org, path, roots.MCP)
		}
		ms := o.f.MeshSessionFor(o.items[org].ID)
		if ms == nil || len(ms.Minted) == 0 || toks[org] != ms.Minted[len(ms.Minted)-1] {
			t.Fatalf("%s's file holds %q, want its own session's id token (%+v)", org, toks[org], ms)
		}
		if toks[org] == machine || strings.Contains(toks[org], machine) {
			t.Fatalf("%s's file holds the machine bearer", org)
		}
	}
	if toks["acme"] == toks["beta"] {
		t.Fatalf("both orgs' sessions hold one bearer %q", toks["acme"])
	}
	for _, other := range []string{"acme", "beta"} {
		roots, _ := o.roots.For(other)
		for org, p := range paths {
			if org != other && strings.HasPrefix(p, roots.MCP) {
				t.Fatalf("%s's file %s is under %s's MCP root", org, p, other)
			}
		}
	}
	opens := o.f.CallsNamed("openMeshSession")
	if len(opens) != 2 {
		t.Fatalf("opens %+v", opens)
	}
	for _, c := range opens {
		org := "acme"
		if c.Input["item_id"] == itemB {
			org = "beta"
		}
		if c.Input["node_id"] != o.nodeOf[org] || c.Input["claim_seq"] != o.items[org].Attempts || c.Bearer != machine {
			t.Fatalf("open %+v, want node %s claim %d on the node's bearer", c.Input, o.nodeOf[org], o.items[org].Attempts)
		}
	}

	// a refresh rewrites each file from its own session alone
	if err := o.tf.Refresh(); err != nil {
		t.Fatal(err)
	}
	if readTok(t, paths["acme"]) != toks["acme"] || readTok(t, paths["beta"]) != toks["beta"] {
		t.Fatal("a refresh crossed the sessions' bearers")
	}

	// acme's session ends: its bearer cleared, its file and dir gone;
	// beta's untouched
	ends["acme"]()
	if o.tf.Bound(item1, 1) {
		t.Fatal("acme's bearer is still bound after its session ended")
	}
	if _, err := os.Stat(filepath.Dir(paths["acme"])); !os.IsNotExist(err) {
		t.Fatalf("acme's token dir is left: %v", err)
	}
	if readTok(t, paths["beta"]) != toks["beta"] || !o.tf.Bound(itemB, 3) {
		t.Fatal("beta's session lost its bearer when acme's ended")
	}
	ends["beta"]()
	if len(o.tf.Kept()) != 0 {
		t.Fatalf("files still kept: %v", o.tf.Kept())
	}
}

// A bearer cleared while its file is still kept is never replaced by
// the machine's: the refresh fails and the file keeps what it held.
func TestClearedSessionBearerNeverFallsBackToTheMachine(t *testing.T) {
	o := newTwoOrgs(t)
	path, unbind, release, err := o.session(t, "acme")
	if err != nil {
		t.Fatal(err)
	}
	defer release(true)
	before := readTok(t, path)
	unbind()
	if err := o.tf.Refresh(); err == nil {
		t.Fatal("a refresh of a cleared session's file succeeded")
	}
	if got := readTok(t, path); got != before || got == machine {
		t.Fatalf("file holds %q after the bearer was cleared", got)
	}
}

// A platform without POST /mesh/v1/sessions on a node serving two orgs:
// the build is refused, naming why, and its file is never written with
// the machine bearer.
func TestNoMeshSessionsOnTwoOrgsRefuses(t *testing.T) {
	o := newTwoOrgs(t)
	o.f.NoMeshSessions = true
	_, _, _, err := o.session(t, "acme")
	rf, ok := MachineBearerRefusalOf(err)
	if !ok || rf.Orgs != 2 || !errors.Is(err, ErrMachineBearerRefused) {
		t.Fatalf("err = %v, want a MachineBearerRefusal of 2 orgs", err)
	}
	want := "mesh session: the platform opens no mesh sessions (POST /mesh/v1/sessions), and a node serving 2 orgs gives no session the machine bearer"
	if err.Error() != want {
		t.Fatalf("reason %q, want %q", err, want)
	}
	// belt and braces: the keeper refuses a build with no bearer bound
	path := filepath.Join(t.TempDir(), harness.TokenFileName)
	if _, err := o.tf.KeepSession(path, harness.KindBuild, harness.SessionMeta{ItemID: item1, Attempt: 1}); !errors.Is(err, ErrMachineBearerRefused) {
		t.Fatalf("KeepSession = %v, want ErrMachineBearerRefused", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a refused build's token file was written")
	}
}

// The same on a one-org node falls back to the machine bearer, said once.
func TestNoMeshSessionsOnOneOrgFallsBack(t *testing.T) {
	o := newTwoOrgs(t)
	o.f.NoMeshSessions = true
	o.r.Scopes = o.r.Scopes[:1]
	one := o.r.Scopes
	o.tf.MultiOrg = func() bool { return len(one) > 1 }
	o.r.Bearers.Orgs = func() int { return len(one) }
	var buf bytes.Buffer
	o.r.Bearers.Logf = log.New(&buf, "", 0).Printf
	roots, err := NewOrgRootSet(t.TempDir(), one.Names())
	if err != nil {
		t.Fatal(err)
	}
	o.r.Roots = roots
	for i := 0; i < 2; i++ {
		path, unbind, release, err := o.session(t, "acme")
		if err != nil {
			t.Fatal(err)
		}
		if got := readTok(t, path); got != machine {
			t.Fatalf("one-org fallback file holds %q, want the machine bearer", got)
		}
		unbind()
		release(true)
	}
	if n := strings.Count(buf.String(), "the platform opens no mesh sessions"); n != 1 {
		t.Fatalf("fallback said %d times:\n%s", n, buf.String())
	}
}

// RunItem on a two-org node whose platform opens no mesh sessions
// releases the item with the named reason, and no session starts.
func TestRunItemOnTwoOrgsWithoutMeshSessionsReleases(t *testing.T) {
	f := seed(t)
	f.NoMeshSessions = true
	h := &fakeHarness{}
	r, _, item, row := runnerOnFake(t, f, h, nil)
	r.Scopes = append(r.Scopes, Scope{Login: "beta", ID: orgB, NodeID: nodeB})
	// sandboxed, so the host fallback's own refusal of a second org
	// (sandbox_required) is not what stops it
	r.Sandbox = &sandbox.Sandbox{Runtime: &sandbox.Fake{}, Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:v1"}, TempDir: t.TempDir()}
	r.Bearers = &SessionBearers{GW: gateway.New(f.URL(), machine), Tokens: &TokenFiles{Token: func(context.Context) (string, error) { return machine, nil }}, Orgs: func() int { return len(r.Scopes) }}
	var buf bytes.Buffer
	r.Log = log.New(&buf, "", 0)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 0 {
		t.Fatalf("a session started: %d jobs", len(h.jobs))
	}
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 || !strings.Contains(rel[0].Input["note"].(string), "a node serving 2 orgs gives no session the machine bearer") {
		t.Fatalf("releases %+v", rel)
	}
	if row(item1)["status"] != "ready" || len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatalf("row %v", row(item1))
	}
}

// And on a one-org node the item runs on the machine bearer as before.
func TestRunItemOnOneOrgWithoutMeshSessionsRuns(t *testing.T) {
	f := seed(t)
	f.NoMeshSessions = true
	h := &fakeHarness{}
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	r.Bearers = &SessionBearers{GW: gateway.New(f.URL(), machine), Tokens: &TokenFiles{Token: func(context.Context) (string, error) { return machine, nil }}, Orgs: func() int { return len(r.Scopes) }}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 || len(f.CallsNamed("releaseWorkItem")) != 0 {
		t.Fatalf("jobs %d, releases %+v", len(h.jobs), f.CallsNamed("releaseWorkItem"))
	}
}

// With the route served, RunItem opens the build's session before the
// harness and the bearer is bound while it runs, cleared after.
func TestRunItemBindsItsOwnBearerWhileItRuns(t *testing.T) {
	f := seed(t)
	tf := &TokenFiles{Token: func(context.Context) (string, error) { return machine, nil }}
	var boundDuring bool
	h := &fakeHarness{}
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	item.Attempts = 0
	r.Bearers = &SessionBearers{GW: gateway.New(f.URL(), machine), Tokens: tf, Audience: MeshMCPURL(f.URL()), Orgs: func() int { return 2 }}
	h.onRun = func(job harness.Job) { boundDuring = tf.Bound(job.Meta.ItemID, job.Meta.Attempt) }
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(f.CallsNamed("openMeshSession")) != 1 || !boundDuring {
		t.Fatalf("opens %d, bound during the run %v", len(f.CallsNamed("openMeshSession")), boundDuring)
	}
	if tf.Bound(item1, 0) {
		t.Fatal("the bearer is still bound after the build")
	}
}

// Until the relay signs its requests (runsheet ADR 064), a one-org
// node with MachineUntilDPoP opens the build's mesh session and mints
// its first id token, but the session reads on the machine bearer, so
// its runsheet server starts; a node serving two orgs keeps the
// session's own token.
func TestOneOrgSessionReadsOnTheMachineBearerUntilDPoP(t *testing.T) {
	for _, tc := range []struct {
		orgs      int
		wantBound bool
	}{{1, false}, {2, true}} {
		f := seed(t)
		tf := &TokenFiles{Token: func(context.Context) (string, error) { return machine, nil }}
		var boundDuring bool
		h := &fakeHarness{}
		r, _, item, _ := runnerOnFake(t, f, h, nil)
		item.Attempts = 0
		var buf bytes.Buffer
		orgs := tc.orgs
		r.Bearers = &SessionBearers{GW: gateway.New(f.URL(), machine), Tokens: tf, Audience: MeshMCPURL(f.URL()),
			Orgs: func() int { return orgs }, MachineUntilDPoP: true, Logf: log.New(&buf, "", 0).Printf}
		h.onRun = func(job harness.Job) { boundDuring = tf.Bound(job.Meta.ItemID, job.Meta.Attempt) }
		if err := r.RunItem(context.Background(), item); err != nil {
			t.Fatal(err)
		}
		if len(h.jobs) != 1 || len(f.CallsNamed("openMeshSession")) != 1 {
			t.Fatalf("%d orgs: jobs %d, opens %d", orgs, len(h.jobs), len(f.CallsNamed("openMeshSession")))
		}
		if ms := f.MeshSessionFor(item1); ms == nil || len(ms.Minted) == 0 {
			t.Fatalf("%d orgs: no id token minted (%+v)", orgs, ms)
		}
		if boundDuring != tc.wantBound {
			t.Fatalf("%d orgs: session token bound during the run %v, want %v", orgs, boundDuring, tc.wantBound)
		}
		if said := strings.Contains(buf.String(), "DPoP-bound"); said == tc.wantBound {
			t.Fatalf("%d orgs: log %q", orgs, buf.String())
		}
	}
}
