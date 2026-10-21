package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
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

// twoJobsKept lays out two jobs of two connections and, in each job's
// kept sessions, one expired kept session and one young one.
func twoJobsKept(t *testing.T, now time.Time) (l *Layout, expired, young []string) {
	t.Helper()
	l = testLayout(t)
	scopes := connected(Scopes{{Login: "acme", ID: "org-a"}, {Login: "grapevine", ID: "org-b"}})
	for _, sc := range scopes {
		j, _, err := openJob(l, scopes, sc.ID, JobMeta{ID: "item-" + sc.Name(), Unit: JobItem, ItemID: "item-" + sc.Name()})
		if err != nil {
			t.Fatal(err)
		}
		expired = append(expired, mkKeptSession(t, j.Kept, sc.Name()+"-old", now.Add(-harness.KeepSessionOutput-time.Hour)))
		young = append(young, mkKeptSession(t, j.Kept, sc.Name()+"-young", now.Add(-time.Hour)))
	}
	return l, expired, young
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

// The kept-sessions sweep removes each job's expired kept session under
// its own kept sessions and keeps each one's young one (ADR-039).
func TestLayoutSweepKeptSessionsCoversEveryJob(t *testing.T) {
	now := time.Now()
	l, expired, young := twoJobsKept(t, now)
	if err := l.SweepKeptSessions(now); err != nil {
		t.Fatal(err)
	}
	assertKeptSwept(t, expired, young)

	var none *Layout
	if err := none.SweepKeptSessions(now); err != nil {
		t.Fatalf("nil layout: %v", err)
	}
}

// The quartermaster's check sweeps every job's kept sessions and a work
// root's own kept-sessions, with no Codex session started in any.
func TestKeptSessionsCheckSweepsEveryJobAndTheWorkRoot(t *testing.T) {
	now := time.Now()
	l, expired, young := twoJobsKept(t, now)
	work := t.TempDir()
	expired = append(expired, mkKeptSession(t, filepath.Join(work, "kept-sessions"), "old", now.Add(-harness.KeepSessionOutput-time.Hour)))
	young = append(young, mkKeptSession(t, filepath.Join(work, "kept-sessions"), "young", now.Add(-time.Hour)))

	check := KeptSessionsCheck(l, work)
	fs := check(context.Background(), now)
	if len(fs) != 1 || fs[0].Kind != "kept_sessions" || fs[0].Severity != "ok" {
		t.Fatalf("findings = %+v", fs)
	}
	assertKeptSwept(t, expired, young)

	// no layout: the work root's alone
	if fs := KeptSessionsCheck(nil, work)(context.Background(), now); fs[0].Severity != "ok" {
		t.Fatalf("nil layout: %+v", fs)
	}
}

// A session's KeptRoot is its own job's; with no layout it is "", the
// harness's old <work root>/kept-sessions.
func TestKeptRootForIsTheJobsOwn(t *testing.T) {
	l := testLayout(t)
	for _, id := range []string{"item-a", "goal-b"} {
		want, _ := l.Job(id)
		got, err := keptRootFor(l, id)
		if err != nil || got != want.Kept {
			t.Errorf("keptRootFor(%s) = %q, %v; want %q", id, got, err, want.Kept)
		}
	}
	if got, err := keptRootFor(l, "../escape"); err == nil {
		t.Errorf("a key with a separator given %q", got)
	}
	if got, err := keptRootFor(nil, "item-a"); err != nil || got != "" {
		t.Errorf("no layout: %q, %v; want \"\"", got, err)
	}
}
