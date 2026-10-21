package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// jobSweepGW is the platform the job sweep reads: items and goals by id.
type jobSweepGW struct {
	Gateway
	items map[string]*gateway.WorkItem
	goals map[string]*gateway.Goal
}

func (g *jobSweepGW) WorkItem(_ context.Context, id string) (*gateway.WorkItem, error) {
	if it, ok := g.items[id]; ok {
		return it, nil
	}
	return nil, gateway.ErrNotFound
}

func (g *jobSweepGW) Goal(_ context.Context, id string) (*gateway.Goal, error) {
	if gl, ok := g.goals[id]; ok {
		return gl, nil
	}
	return nil, gateway.ErrNotFound
}

// jobWorld is a temp layout serving acme by conn-acme, and the
// platform's answer for its jobs.
type jobWorld struct {
	t      *testing.T
	l      *Layout
	scopes Scopes
	gw     *jobSweepGW
	logs   []string
}

func newJobWorld(t *testing.T) *jobWorld {
	return &jobWorld{t: t, l: testLayout(t), scopes: connected(Scopes{{Login: "acme", ID: "org-a"}}),
		gw: &jobSweepGW{items: map[string]*gateway.WorkItem{}, goals: map[string]*gateway.Goal{}}}
}

// job opens a job of m in acme and writes a file in it.
func (w *jobWorld) job(m JobMeta) JobDirs {
	w.t.Helper()
	j, err := openJob(w.l, w.scopes, "org-a", m)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.MkdirAll(j.Checkout(), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.Checkout(), "main.go"), []byte("package main\n"), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return j
}

func (w *jobWorld) item(id, status, statusAt string) JobDirs {
	w.gw.items[id] = &gateway.WorkItem{ID: id, Seq: 1, Status: status, StatusAt: statusAt}
	return w.job(JobMeta{ID: id, Unit: JobItem, ItemID: id, Label: "ship-it/" + id})
}

func (w *jobWorld) plan(goalID, status string) JobDirs {
	w.gw.goals[goalID] = &gateway.Goal{ID: goalID, Slug: "g", Status: status}
	return w.job(JobMeta{ID: goalID, Unit: JobPlan, GoalID: goalID, Label: "g"})
}

func (w *jobWorld) sweep(connections map[string]bool, live LiveSessions) WorkSweepPlan {
	w.t.Helper()
	plan, err := PlanJobSweep(context.Background(), w.gw, w.l, connections, live, sweepNow, func(f string, a ...any) {
		w.logs = append(w.logs, strings.TrimSpace(fmt.Sprintf(f, a...)))
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return plan
}

func removedDirs(p WorkSweepPlan) map[string]bool {
	out := map[string]bool{}
	for _, r := range p.Removals {
		out[r.Path] = true
	}
	return out
}

// The job sweep removes a finished item's job, a failed one's after a
// week, a plan's once its goal is not planning, a batch's after
// BatchJobKeep, and a job of a connection the node no longer has; it
// keeps a running item's, a recently failed one's, a planning goal's, a
// research's and anything a live session holds (ADR-039).
func TestPlanJobSweepJudgesEachJob(t *testing.T) {
	w := newJobWorld(t)
	merged := w.item("item-merged", "merged", "")
	running := w.item("item-running", "running", "")
	oldFail := w.item("item-oldfail", "failed", sweepNow.Add(-8*24*time.Hour).Format(time.RFC3339))
	newFail := w.item("item-newfail", "failed", sweepNow.Add(-time.Hour).Format(time.RFC3339))
	liveMerged := w.item("item-live", "merged", "")
	donePlan := w.plan("goal-done", "running")
	planning := w.plan("goal-planning", "planning")
	research := w.job(JobMeta{ID: "dir-1", Unit: JobResearch, DirectionID: "dir-1"})
	oldBatch := w.job(JobMeta{ID: "batch-old", Unit: JobBatch, BatchID: "batch-old", Started: sweepNow.Add(-7 * time.Hour)})
	newBatch := w.job(JobMeta{ID: "batch-new", Unit: JobBatch, BatchID: "batch-new", Started: sweepNow.Add(-time.Hour)})
	// a directory with no job.json is left
	stray := filepath.Join(w.l.JobsDir(), "stray")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}

	got := removedDirs(w.sweep(map[string]bool{"conn-acme": true}, LiveSessions{IDs: map[string]bool{"item-live": true}}))
	for _, j := range []JobDirs{merged, oldFail, donePlan, oldBatch} {
		if !got[j.Dir] {
			t.Errorf("%s kept, want removed", j.ID)
		}
	}
	for _, j := range []JobDirs{running, newFail, liveMerged, planning, research, newBatch} {
		if got[j.Dir] {
			t.Errorf("%s removed, want kept", j.ID)
		}
	}
	if got[stray] {
		t.Error("a directory with no job.json was removed")
	}

	// the node's connection gone: every job not held live goes
	gone := removedDirs(w.sweep(map[string]bool{"conn-other": true}, LiveSessions{IDs: map[string]bool{"item-live": true}}))
	for _, j := range []JobDirs{running, planning, research, newBatch} {
		if !gone[j.Dir] {
			t.Errorf("%s of a gone connection kept", j.ID)
		}
	}
	if gone[liveMerged.Dir] {
		t.Error("a live job of a gone connection removed")
	}
	// connections unknown (nil): judged by the platform alone
	if unknown := removedDirs(w.sweep(nil, LiveSessions{})); unknown[running.Dir] {
		t.Error("a running item's job removed with connections unknown")
	}
}

// A job whose session dir holds a live session is kept however its item
// stands; a dead one's is not.
func TestPlanJobSweepKeepsAJobWithALiveSession(t *testing.T) {
	w := newJobWorld(t)
	j := w.item("item-1", "merged", "")
	sess := sessionunit.Dir(j.Checkout())
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	st := sessionunit.State{Kind: string(harness.KindBuild), ItemID: "item-1", NodeID: "node-a", Runner: sessionunit.Detached{}.Name(), PID: os.Getpid(), StartedAt: sweepNow}
	if err := sessionunit.WriteState(sess, st); err != nil {
		t.Fatal(err)
	}
	if got := removedDirs(w.sweep(nil, LiveSessions{})); got[j.Dir] {
		t.Fatalf("a job with a live session removed:\n%s", strings.Join(w.logs, "\n"))
	}
}

// Each kept job's kept sessions past harness.KeepSessionOutput go, the
// job itself stays.
func TestPlanJobSweepSweepsAKeptJobsKeptSessions(t *testing.T) {
	w := newJobWorld(t)
	j := w.item("item-1", "running", "")
	old := mkKeptSession(t, j.Kept, "old", sweepNow.Add(-harness.KeepSessionOutput-time.Hour))
	young := mkKeptSession(t, j.Kept, "young", sweepNow.Add(-time.Hour))
	got := removedDirs(w.sweep(nil, LiveSessions{}))
	if !got[old] || got[young] || got[j.Dir] {
		t.Fatalf("removals = %v", got)
	}
}
