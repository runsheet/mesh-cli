package supervisor

import (
	"context"
	"fmt"
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
	for in, want := range map[string]Mode{"": Off, "off": Off, "warn": Warn, "report": Warn, "Cancel": Cancel, "tidy": Tidy, "TIDY": Tidy} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseMode("kill"); err == nil {
		t.Error("ParseMode(kill) passed")
	}
}

// Mode tidy never escalates or stalls a quiet lease, however long it
// stays quiet -- only mode cancel manages that ladder.
func TestTidyNeverEscalatesOrStalls(t *testing.T) {
	now := t0
	f := &fleet{canEscal: true, now: func() time.Time { return now }, live: []Live{{LeaseID: "l", Running: true, ProgressAt: t0}}}
	s := &Supervisor{Fleet: f, Mode: Tidy, QuietAfter: 10 * time.Minute, Now: func() time.Time { return now }}
	now = t0.Add(25 * time.Minute)
	s.Look()
	if len(f.escalated) != 0 || len(f.stalled) != 0 {
		t.Fatalf("tidy acted: escalated %v stalled %v", f.escalated, f.stalled)
	}
}

// Mode tidy reconciles exactly as mode cancel does: the orphan pod,
// job directory and grant no actor holds are removed.
func TestTidyReconcilesLikeCancel(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "lse_orphan"), 0o700)
	old := t0.Add(-time.Hour)
	os.Chtimes(filepath.Join(root, "lse_orphan"), old, old)
	sb := &sandbox.Fake{Standing: []sandbox.PodInfo{{Name: "p", Labels: map[string]string{"mesh.v1.lease": "gone"}}}}
	f := &fleet{now: func() time.Time { return t0 }, grants: []string{"gone"}}
	s := &Supervisor{Fleet: f, Mode: Tidy, Sandbox: sb, PodLabel: "mesh.v1.lease", LeaseRoot: root, Now: func() time.Time { return t0 }}
	got := s.Reconcile(context.Background())
	if !slices.Equal(got.PodsNoActor, []string{"p"}) || len(got.Workspaces) != 1 || !slices.Equal(got.Grants, []string{"gone"}) {
		t.Fatalf("found %+v", got)
	}
	if !slices.Equal(sb.Removed, []string{"p"}) {
		t.Fatalf("removed %v", sb.Removed)
	}
	if _, err := os.Stat(filepath.Join(root, "lse_orphan")); !os.IsNotExist(err) {
		t.Fatal("tidy left the orphan directory")
	}
	if !slices.Equal(f.closed, []string{"gone"}) {
		t.Fatalf("closed grants %v", f.closed)
	}
}

// SetMode takes effect at once: a Supervisor built with Mode Warn, then
// switched to Tidy by SetMode before its first Reconcile call, acts
// like Tidy on that very call.
func TestSetModeTakesEffectImmediately(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "lse_orphan"), 0o700)
	old := t0.Add(-time.Hour)
	os.Chtimes(filepath.Join(root, "lse_orphan"), old, old)
	f := &fleet{now: func() time.Time { return t0 }}
	s := &Supervisor{Fleet: f, Mode: Warn, LeaseRoot: root, Now: func() time.Time { return t0 }}
	s.SetMode(Tidy)
	got := s.Reconcile(context.Background())
	if len(got.Workspaces) != 1 {
		t.Fatalf("workspaces %v, want the orphan found", got.Workspaces)
	}
	if _, err := os.Stat(filepath.Join(root, "lse_orphan")); !os.IsNotExist(err) {
		t.Fatal("SetMode(Tidy) did not take effect: the orphan directory is still there")
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

// fakeEgress is a test's EgressRuntime.
type fakeEgress struct {
	procs   []EgressProcess
	listErr error
	stopErr map[int]error
	stopped []int
}

func (e *fakeEgress) List() ([]EgressProcess, error) { return e.procs, e.listErr }
func (e *fakeEgress) Stop(pid int) error {
	e.stopped = append(e.stopped, pid)
	if e.stopErr != nil {
		return e.stopErr[pid]
	}
	return nil
}

// The reconcile stops a lease-egress process no live lease's EgressPID
// names, and leaves one a live lease holds (ADR-051 decision 3); mode
// warn finds the same orphan but stops nothing.
func TestReconcileEgress(t *testing.T) {
	f := &fleet{now: func() time.Time { return t0 }, live: []Live{
		{LeaseID: "held", Running: true, EgressPID: 111},
		{LeaseID: "none", Running: true},
	}}
	eg := &fakeEgress{procs: []EgressProcess{{PID: 111}, {PID: 222}}}
	s := &Supervisor{Fleet: f, Mode: Cancel, Egress: eg, Now: func() time.Time { return t0 }}
	got := s.Reconcile(context.Background())
	if !slices.Equal(got.EgressNoActor, []int{222}) {
		t.Fatalf("egress no actor: %v", got.EgressNoActor)
	}
	if !slices.Equal(eg.stopped, []int{222}) {
		t.Fatalf("stopped: %v, want only the orphan", eg.stopped)
	}

	eg2 := &fakeEgress{procs: []EgressProcess{{PID: 111}, {PID: 222}}}
	s2 := &Supervisor{Fleet: f, Mode: Warn, Egress: eg2, Now: func() time.Time { return t0 }}
	got2 := s2.Reconcile(context.Background())
	if !slices.Equal(got2.EgressNoActor, []int{222}) {
		t.Fatalf("warn egress no actor: %v", got2.EgressNoActor)
	}
	if len(eg2.stopped) != 0 {
		t.Fatalf("warn stopped %v, want none", eg2.stopped)
	}
}

// A nil Egress runs no egress reconcile at all, and a List error is
// reported without stopping anything.
func TestReconcileEgressNilAndError(t *testing.T) {
	f := &fleet{now: func() time.Time { return t0 }}
	s := &Supervisor{Fleet: f, Mode: Cancel, Now: func() time.Time { return t0 }}
	got := s.Reconcile(context.Background())
	if got.EgressNoActor != nil {
		t.Fatalf("egress no actor with no runtime: %v", got.EgressNoActor)
	}

	eg := &fakeEgress{listErr: errListFailed}
	s2 := &Supervisor{Fleet: f, Mode: Cancel, Egress: eg, Now: func() time.Time { return t0 }}
	got2 := s2.Reconcile(context.Background())
	if len(got2.Errors) != 1 || !strings.Contains(got2.Errors[0], "listing egress processes") {
		t.Fatalf("errors: %v", got2.Errors)
	}
	if len(eg.stopped) != 0 {
		t.Fatalf("stopped on a list error: %v", eg.stopped)
	}
}

var errListFailed = fmt.Errorf("the runtime refused")

// A stray egress process -- a leftover lease's actor.json names it, no
// actor holds the lease and no hand-off names it -- is stopped and
// counted, though the runtime's listing missed it; one a hand-off names
// (by its actor.json or its handoff.json, listed or not), one held by a
// live actor and a directory with no actor.json at all are left alone.
func TestReconcileEgressFromActorRecords(t *testing.T) {
	root := t.TempDir()
	write := func(dir, name, data string) {
		os.MkdirAll(filepath.Join(root, dir), 0o700)
		os.WriteFile(filepath.Join(root, dir, name), []byte(data), 0o600)
	}
	write("lse_stray", "actor.json", `{"egressPid":4242,"egressControl":"/tmp/x.sock"}`)
	write("lse_handoff", "actor.json", `{"egressPid":4343,"egressControl":"/tmp/y.sock"}`)
	write("lse_handoff", "handoff.json", `{"egressPid":4444}`)
	write("lse_no_pid", "actor.json", `{"egressPid":0}`)
	write("lse_live", "actor.json", `{"egressPid":4545}`)
	os.MkdirAll(filepath.Join(root, "lse_bare"), 0o700)
	f := &fleet{now: func() time.Time { return t0 }, live: []Live{{LeaseID: "lse_live", EgressPID: 4545}}}
	eg := &fakeEgress{procs: []EgressProcess{{PID: 4343}, {PID: 4444}, {PID: 4545}}}
	s := &Supervisor{Fleet: f, Mode: Cancel, LeaseRoot: root, Egress: eg, Now: func() time.Time { return t0 }}
	got := s.Reconcile(context.Background())
	if !slices.Equal(got.EgressNoActor, []int{4242}) {
		t.Fatalf("egress no actor: %v", got.EgressNoActor)
	}
	if !slices.Equal(eg.stopped, []int{4242}) {
		t.Fatalf("stopped %v", eg.stopped)
	}
	if !strings.Contains(got.Line(), "stopped 1 egress process(es) with no actor and no hand-off") {
		t.Fatalf("line %q", got.Line())
	}

	eg.stopped = nil
	eg.stopErr = map[int]error{4242: fmt.Errorf("boom")}
	got = s.Reconcile(context.Background())
	found := false
	for _, e := range got.Errors {
		if strings.Contains(e, "boom") {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors %v missing the stop error", got.Errors)
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
