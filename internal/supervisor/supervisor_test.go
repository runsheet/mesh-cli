package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// fleet is a test's actors.
type fleet struct {
	live      []Live
	canEscal  bool
	escalated []string
	stalled   []string
	grants    []string
	closed    []string
	now       func() time.Time
}

func (f *fleet) Live() []Live { return append([]Live(nil), f.live...) }
func (f *fleet) Escalate(id, why string) bool {
	for i := range f.live {
		if f.live[i].LeaseID == id {
			f.live[i].EscalatedAt = f.now()
		}
	}
	f.escalated = append(f.escalated, id)
	return f.canEscal
}
func (f *fleet) Stall(id, why string) { f.stalled = append(f.stalled, id) }
func (f *fleet) Grants() []string     { return f.grants }
func (f *fleet) CloseGrant(id string) { f.closed = append(f.closed, id) }

var t0 = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

// A quiet running session is escalated after QuietAfter, then stopped
// stalled after QuietAfter more; progress in between starts it over. A
// session not running (preparing, reporting) is never quiet.
func TestLivenessEscalatesThenStalls(t *testing.T) {
	now := t0
	f := &fleet{canEscal: true, now: func() time.Time { return now }}
	f.live = []Live{
		{LeaseID: "quiet", Running: true, ProgressAt: t0},
		{LeaseID: "busy", Running: true, ProgressAt: t0},
		{LeaseID: "preparing", ProgressAt: t0},
	}
	var lines []string
	s := &Supervisor{Fleet: f, Mode: Cancel, QuietAfter: 10 * time.Minute, Now: func() time.Time { return now },
		Logf: func(format string, a ...any) { lines = append(lines, format) }}
	now = t0.Add(9 * time.Minute)
	s.Look()
	if len(f.escalated)+len(f.stalled) != 0 {
		t.Fatal("acted before QuietAfter")
	}
	now = t0.Add(11 * time.Minute)
	s.Look()
	if !slices.Equal(f.escalated, []string{"quiet", "busy"}) || len(f.stalled) != 0 {
		t.Fatalf("escalated %v stalled %v", f.escalated, f.stalled)
	}
	// busy shows progress after its escalation
	f.live[1].ProgressAt = t0.Add(12 * time.Minute)
	now = t0.Add(22 * time.Minute)
	s.Look()
	if !slices.Equal(f.stalled, []string{"quiet"}) {
		t.Fatalf("stalled %v, want quiet alone", f.stalled)
	}
	// busy is escalated afresh only once it is quiet again for QuietAfter
	if len(f.escalated) != 3 || f.escalated[2] != "busy" {
		t.Fatalf("escalated %v", f.escalated)
	}
}

// A lease with nothing to escalate to is still stopped once quiet past
// a second window.
func TestNothingToEscalateToStillStalls(t *testing.T) {
	now := t0
	f := &fleet{now: func() time.Time { return now }, live: []Live{{LeaseID: "l", Running: true, ProgressAt: t0}}}
	s := &Supervisor{Fleet: f, Mode: Cancel, QuietAfter: time.Minute, Now: func() time.Time { return now }}
	now = t0.Add(time.Minute)
	s.Look()
	now = t0.Add(2 * time.Minute)
	s.Look()
	if !slices.Equal(f.stalled, []string{"l"}) {
		t.Fatalf("stalled %v", f.stalled)
	}
}

// Mode warn logs a quiet lease once at each threshold and acts on none.
func TestWarnOnlyLogs(t *testing.T) {
	now := t0
	f := &fleet{canEscal: true, now: func() time.Time { return now }, live: []Live{{LeaseID: "l", Running: true, ProgressAt: t0}}}
	var lines []string
	s := &Supervisor{Fleet: f, Mode: Warn, QuietAfter: 10 * time.Minute, LookEvery: 15 * time.Second, Now: func() time.Time { return now },
		Logf: func(format string, a ...any) { lines = append(lines, format) }}
	for m := 0; m <= 25*60; m += 15 {
		now = t0.Add(time.Duration(m) * time.Second)
		s.Look()
	}
	if len(f.escalated)+len(f.stalled) != 0 {
		t.Fatal("warn acted")
	}
	if len(lines) != 2 || !strings.Contains(lines[0], "would be escalated") || !strings.Contains(lines[1], "would be stopped") {
		t.Fatalf("logged %q", lines)
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Off, "off": Off, "warn": Warn, "report": Warn, "Cancel": Cancel} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseMode("kill"); err == nil {
		t.Error("ParseMode(kill) passed")
	}
}

// The reconcile removes the node's pods no actor holds and those whose
// agent ended leaving a sidecar running, the job directories and grants
// no actor holds; another's pods, a pod whose agent runs, a pod whose
// session runs on the host (no agent) and a young directory are left.
func TestReconcile(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"lse_live", "lse_orphan", "lse_young"} {
		os.MkdirAll(filepath.Join(root, d), 0o700)
	}
	old := t0.Add(-time.Hour)
	os.Chtimes(filepath.Join(root, "lse_orphan"), old, old)
	os.Chtimes(filepath.Join(root, "lse_live"), old, old)
	os.Chtimes(filepath.Join(root, "lse_young"), t0, t0)
	pod := func(name, lease string) sandbox.PodInfo {
		return sandbox.PodInfo{Name: name, Labels: map[string]string{"mesh.v1.lease": lease}}
	}
	sb := &sandbox.Fake{
		Standing: []sandbox.PodInfo{
			pod("p-orphan", "lse_gone"),
			pod("p-sidecar", "lse_live"),
			pod("p-ok", "lse_ok"),
			pod("p-host", "lse_host"),
			{Name: "legacy-pod", Labels: map[string]string{"mesh.session": "x"}},
		},
		Running: []sandbox.Container{
			{Name: "p-sidecar-agent", Pod: "p-sidecar"},
			{Name: "p-sidecar-postgres", Pod: "p-sidecar", Running: true},
			{Name: "p-ok-agent", Pod: "p-ok", Running: true},
			{Name: "p-ok-postgres", Pod: "p-ok", Running: true},
			{Name: "p-host-postgres", Pod: "p-host", Running: true},
		},
	}
	f := &fleet{now: func() time.Time { return t0 }, grants: []string{"lse_live", "lse_stale"}, live: []Live{
		{LeaseID: "lse_live", Running: true, Pod: "p-sidecar"},
		{LeaseID: "lse_ok", Running: true, Pod: "p-ok", Adopted: true},
		{LeaseID: "lse_host", Running: true, Pod: "p-host"},
	}}
	s := &Supervisor{Fleet: f, Mode: Cancel, Sandbox: sb, PodLabel: "mesh.v1.lease", LeaseRoot: root, Now: func() time.Time { return t0 }}
	got := s.Reconcile(context.Background())
	if !slices.Equal(got.PodsNoActor, []string{"p-orphan"}) || !slices.Equal(got.PodsSidecarOnly, []string{"p-sidecar"}) {
		t.Fatalf("pods %v / %v", got.PodsNoActor, got.PodsSidecarOnly)
	}
	if !slices.Equal(sb.Removed, []string{"p-orphan", "p-sidecar"}) {
		t.Fatalf("removed %v", sb.Removed)
	}
	if len(got.Workspaces) != 1 || filepath.Base(got.Workspaces[0]) != "lse_orphan" {
		t.Fatalf("workspaces %v", got.Workspaces)
	}
	if _, err := os.Stat(filepath.Join(root, "lse_orphan")); !os.IsNotExist(err) {
		t.Fatal("the orphan directory is still there")
	}
	if _, err := os.Stat(filepath.Join(root, "lse_young")); err != nil {
		t.Fatal("the young directory was removed")
	}
	if !slices.Equal(f.closed, []string{"lse_stale"}) {
		t.Fatalf("closed grants %v", f.closed)
	}
	if got.Actors != 3 || got.Adopted != 1 {
		t.Fatalf("counted %d actors, %d adopted", got.Actors, got.Adopted)
	}
}

// Mode warn reconciles without removing anything.
func TestReconcileWarnRemovesNothing(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "lse_orphan"), 0o700)
	old := t0.Add(-time.Hour)
	os.Chtimes(filepath.Join(root, "lse_orphan"), old, old)
	sb := &sandbox.Fake{Standing: []sandbox.PodInfo{{Name: "p", Labels: map[string]string{"mesh.v1.lease": "gone"}}}}
	f := &fleet{now: func() time.Time { return t0 }, grants: []string{"gone"}}
	s := &Supervisor{Fleet: f, Mode: Warn, Sandbox: sb, PodLabel: "mesh.v1.lease", LeaseRoot: root, Now: func() time.Time { return t0 }}
	got := s.Reconcile(context.Background())
	if len(got.PodsNoActor) != 1 || len(got.Workspaces) != 1 || len(got.Grants) != 1 {
		t.Fatalf("found %+v", got)
	}
	if len(sb.Removed) != 0 || len(f.closed) != 0 {
		t.Fatal("warn removed something")
	}
	if _, err := os.Stat(filepath.Join(root, "lse_orphan")); err != nil {
		t.Fatal("warn removed a directory")
	}
}
