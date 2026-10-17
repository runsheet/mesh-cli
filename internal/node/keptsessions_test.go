package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// mkKeptSession writes a kept session under kept as the harness keeps
// one: its note (state.json) naming when it started, and its stream.
func mkKeptSession(t *testing.T, kept, name string, started time.Time) string {
	t.Helper()
	dir := filepath.Join(kept, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"kind": "evaluate", "startedAt": started})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionunit.StateFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionunit.OutputFile), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// twoOrgsKept lays out two orgs' roots and, under each org's Kept, one
// expired kept session and one young one.
func twoOrgsKept(t *testing.T, now time.Time) (roots *OrgRootSet, expired, young []string) {
	t.Helper()
	roots, err := NewOrgRootSet(t.TempDir(), []string{"acme", "grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range roots.Orgs() {
		r, err := roots.For(org)
		if err != nil {
			t.Fatal(err)
		}
		expired = append(expired, mkKeptSession(t, r.Kept, org+"-old", now.Add(-25*time.Hour)))
		young = append(young, mkKeptSession(t, r.Kept, org+"-young", now.Add(-time.Hour)))
	}
	return roots, expired, young
}

func assertKeptSwept(t *testing.T, expired, young []string) {
	t.Helper()
	for _, d := range expired {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("expired kept session %s still there (err %v)", d, err)
		}
	}
	for _, d := range young {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("young kept session %s removed: %v", d, err)
		}
	}
}

// The kept-sessions sweep, given two orgs' roots, removes each org's
// expired kept session under its own Kept and keeps each one's young
// one (ADR-034).
func TestOrgRootSetSweepKeptSessionsCoversEveryOrg(t *testing.T) {
	now := time.Now()
	roots, expired, young := twoOrgsKept(t, now)
	if err := roots.SweepKeptSessions(now); err != nil {
		t.Fatal(err)
	}
	assertKeptSwept(t, expired, young)

	var none *OrgRootSet
	if err := none.SweepKeptSessions(now); err != nil {
		t.Fatalf("nil roots: %v", err)
	}
}

// The quartermaster's check sweeps every org's Kept and the work root's
// own kept-sessions, with no Codex session started for either org.
func TestKeptSessionsCheckSweepsEveryOrgAndTheWorkRoot(t *testing.T) {
	now := time.Now()
	roots, expired, young := twoOrgsKept(t, now)
	work := t.TempDir()
	expired = append(expired, mkKeptSession(t, filepath.Join(work, "kept-sessions"), "old", now.Add(-25*time.Hour)))
	young = append(young, mkKeptSession(t, filepath.Join(work, "kept-sessions"), "young", now.Add(-time.Hour)))

	check := KeptSessionsCheck(func() *OrgRootSet { return roots }, work)
	fs := check(context.Background(), now)
	if len(fs) != 1 || fs[0].Kind != "kept_sessions" || fs[0].Severity != "ok" {
		t.Fatalf("findings = %+v", fs)
	}
	assertKeptSwept(t, expired, young)

	// no roots laid out: the work root's alone
	if fs := KeptSessionsCheck(func() *OrgRootSet { return nil }, work)(context.Background(), now); fs[0].Severity != "ok" {
		t.Fatalf("nil roots: %+v", fs)
	}
}

// A session's KeptRoot is its own org's Kept; an org the node does not
// serve is refused, never given another's; with no roots laid out it is
// "", the harness's old <work root>/kept-sessions.
func TestKeptRootForIsTheOrgsOwn(t *testing.T) {
	roots, err := NewOrgRootSet(t.TempDir(), []string{"acme", "grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	scopes := Scopes{{Login: "acme", ID: "org-a"}, {Login: "grapevine", ID: "org-b"}}
	for _, c := range []struct{ id, org string }{{"org-a", "acme"}, {"org-b", "grapevine"}} {
		want, _ := roots.For(c.org)
		got, err := keptRootFor(roots, scopes, c.id)
		if err != nil || got != want.Kept {
			t.Errorf("keptRootFor(%s) = %q, %v; want %q", c.id, got, err, want.Kept)
		}
		got, err = keptRootOfScope(roots, Scope{Login: c.org, ID: c.id})
		if err != nil || got != want.Kept {
			t.Errorf("keptRootOfScope(%s) = %q, %v; want %q", c.org, got, err, want.Kept)
		}
	}
	if got, err := keptRootFor(roots, scopes, "org-c"); err == nil {
		t.Errorf("unserved org id given %q", got)
	}
	if got, err := keptRootOfScope(roots, Scope{Login: "other", ID: "org-c"}); err == nil {
		t.Errorf("unserved scope given %q", got)
	}
	if got, err := keptRootFor(nil, scopes, "org-c"); err != nil || got != "" {
		t.Errorf("no roots: %q, %v; want \"\"", got, err)
	}
	if got, err := keptRootOfScope(nil, Scope{Login: "acme"}); err != nil || got != "" {
		t.Errorf("no roots (scope): %q, %v; want \"\"", got, err)
	}
}
