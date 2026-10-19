package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// sweepGW is the platform the work sweep reads: each org's goals and
// each goal's items, or an error for either.
type sweepGW struct {
	Gateway
	goals    map[string][]gateway.Goal
	goalsErr map[string]error
	items    map[string][]gateway.WorkItem
	itemsErr map[string]error
}

func (g *sweepGW) Goals(_ context.Context, org string) ([]gateway.Goal, error) {
	if err := g.goalsErr[org]; err != nil {
		return nil, err
	}
	return g.goals[org], nil
}

func (g *sweepGW) GoalWorkItems(_ context.Context, goal string) ([]gateway.WorkItem, error) {
	if err := g.itemsErr[goal]; err != nil {
		return nil, err
	}
	return g.items[goal], nil
}

var sweepNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// sweepWorld is a temp work root and the platform's answer for it.
type sweepWorld struct {
	t    *testing.T
	root string
	gw   *sweepGW
	live LiveSessions
	logs []string
}

func newSweepWorld(t *testing.T) *sweepWorld {
	return &sweepWorld{t: t, root: t.TempDir(), gw: &sweepGW{
		goals: map[string][]gateway.Goal{}, goalsErr: map[string]error{},
		items: map[string][]gateway.WorkItem{}, itemsErr: map[string]error{},
	}}
}

// goal serves a goal of org-1 with its status.
func (w *sweepWorld) goal(slug, status string) {
	w.gw.goals["org-1"] = append(w.gw.goals["org-1"], gateway.Goal{ID: "goal-" + slug, Slug: slug, Status: status})
}

// item serves an item of the goal and writes its checkout, session dir
// and attestation copy.
func (w *sweepWorld) item(slug string, seq int, status, statusAt string) {
	w.gw.items["goal-"+slug] = append(w.gw.items["goal-"+slug], gateway.WorkItem{
		ID: fmt.Sprintf("item-%s-%d", slug, seq), GoalID: "goal-" + slug, Seq: seq, Status: status, StatusAt: statusAt,
	})
	w.entries(slug, seq)
}

// entries writes <slug>/<seq>, <seq>.session and <seq>.attestation.json.
func (w *sweepWorld) entries(slug string, seq int) {
	co := filepath.Join(w.root, slug, fmt.Sprint(seq))
	w.file(filepath.Join(co, "main.go"), "package main\n")
	w.file(filepath.Join(co+".session", sessionunit.OutputFile), "{}\n")
	w.file(co+".attestation.json", "{}\n")
}

func (w *sweepWorld) file(path, body string) {
	w.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *sweepWorld) logf(format string, a ...any) {
	w.logs = append(w.logs, fmt.Sprintf(format, a...))
}

// plan runs PlanWorkSweep on org-1.
func (w *sweepWorld) plan() WorkSweepPlan {
	w.t.Helper()
	p, err := PlanWorkSweep(context.Background(), w.gw, w.root, []string{"org-1"}, w.live, sweepNow, w.logf)
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

// sweep plans and applies a pass.
func (w *sweepWorld) sweep() WorkSweepPlan {
	p := w.plan()
	ApplyWorkSweep(p, w.logf)
	return p
}

func (w *sweepWorld) gone(rel ...string) {
	w.t.Helper()
	for _, r := range rel {
		if _, err := os.Lstat(filepath.Join(w.root, r)); !os.IsNotExist(err) {
			w.t.Errorf("%s still there (err %v); log:\n%s", r, err, strings.Join(w.logs, "\n"))
		}
	}
}

func (w *sweepWorld) kept(rel ...string) {
	w.t.Helper()
	for _, r := range rel {
		if _, err := os.Lstat(filepath.Join(w.root, r)); err != nil {
			w.t.Errorf("%s removed: %v; log:\n%s", r, err, strings.Join(w.logs, "\n"))
		}
	}
}

func (w *sweepWorld) logged(want string) {
	w.t.Helper()
	if !slices.ContainsFunc(w.logs, func(l string) bool { return strings.Contains(l, want) }) {
		w.t.Errorf("no log line with %q; log:\n%s", want, strings.Join(w.logs, "\n"))
	}
}

// itemEntries is an item's three entries under its goal.
func itemEntries(slug string, seq int) []string {
	co := filepath.Join(slug, fmt.Sprint(seq))
	return []string{co, co + ".session", co + ".attestation.json"}
}

func TestWorkSweepRemovesMergedCancelledAndSupersededItems(t *testing.T) {
	w := newSweepWorld(t)
	w.goal("g", "running")
	w.item("g", 1, "merged", "")
	w.item("g", 2, "cancelled", "")
	w.item("g", 3, "superseded", "")
	w.sweep()
	for seq := 1; seq <= 3; seq++ {
		w.gone(itemEntries("g", seq)...)
	}
	w.logged("sweep: removed g/1 (item 1 merged, ")
	w.logged("sweep: removed g/2.session (item 2 cancelled, ")
	w.logged("sweep: removed g/3.attestation.json (item 3 superseded, ")
	w.logged("sweep: removed 9 entries, ")
}

func TestWorkSweepKeepsReviewClaimedRunningEvaluatingAndHeldItems(t *testing.T) {
	w := newSweepWorld(t)
	w.goal("g", "running")
	for i, st := range []string{"review", "claimed", "running", "evaluating", "held", "ready"} {
		w.item("g", i+1, st, sweepNow.Add(-30*24*time.Hour).Format(time.RFC3339))
	}
	if p := w.sweep(); len(p.Removals) != 0 {
		t.Fatalf("removals %v", p.Removals)
	}
	for seq := 1; seq <= 6; seq++ {
		w.kept(itemEntries("g", seq)...)
	}
}

func TestWorkSweepRemovesAFailedItemAfterAWeek(t *testing.T) {
	w := newSweepWorld(t)
	w.goal("g", "running")
	w.item("g", 1, "failed", sweepNow.Add(-6*24*time.Hour).Format(time.RFC3339))
	w.item("g", 2, "failed", sweepNow.Add(-8*24*time.Hour).Format(time.RFC3339))
	w.sweep()
	w.kept(itemEntries("g", 1)...)
	w.gone(itemEntries("g", 2)...)
	w.logged("sweep: removed g/2 (item 2 failed 8d ago, ")
}

func TestWorkSweepKeepsAFailedItemWithNoStatusAtAndLogsIt(t *testing.T) {
	w := newSweepWorld(t)
	w.goal("g", "running")
	w.item("g", 1, "failed", "")
	w.sweep()
	w.kept(itemEntries("g", 1)...)
	n := 0
	for _, l := range w.logs {
		if l == "sweep: g/1 left, item failed with no statusAt (an older platform)" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("no-statusAt line %d times, want once per pass; log:\n%s", n, strings.Join(w.logs, "\n"))
	}
}

func TestWorkSweepRemovesADoneAndACancelledGoalWhole(t *testing.T) {
	w := newSweepWorld(t)
	w.goal("done-goal", "done")
	w.item("done-goal", 1, "merged", "")
	w.item("done-goal", 2, "review", "")
	w.file(filepath.Join(w.root, "done-goal", "plan", "x"), "x")
	w.file(filepath.Join(w.root, "done-goal", "0", "goal.md"), "x")
	w.goal("off-goal", "cancelled")
	w.item("off-goal", 1, "ready", "")
	w.sweep()
	w.gone("done-goal", "off-goal")
	w.logged("sweep: removed done-goal (goal done, ")
	w.logged("sweep: removed off-goal (goal cancelled, ")
}

func TestWorkSweepPlanDirGoesOnceTheGoalIsNotPlanning(t *testing.T) {
	w := newSweepWorld(t)
	w.goal("planning-goal", "planning")
	w.file(filepath.Join(w.root, "planning-goal", "plan", "x"), "x")
	w.goal("running-goal", "running")
	w.file(filepath.Join(w.root, "running-goal", "plan", "x"), "x")
	w.sweep()
	w.kept("planning-goal/plan")
	w.gone("running-goal/plan")
	w.logged("sweep: removed running-goal/plan (goal running, not planning, ")
}

func TestWorkSweepKeepsALiveSessionsDirectoryThePlatformCallsMerged(t *testing.T) {
	w := newSweepWorld(t)
	w.goal("g", "running")
	w.item("g", 1, "merged", "") // its session's pid is alive
	w.item("g", 2, "merged", "") // its id is in the live set
	w.item("g", 3, "merged", "")
	if err := sessionunit.WriteState(filepath.Join(w.root, "g", "1.session"), sessionunit.State{
		ItemID: "item-g-1", Kind: "build", Runner: sessionunit.Detached{}.Name(), PID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	w.goal("adopted", "done")
	w.item("adopted", 1, "merged", "")
	w.live = LiveSessions{IDs: map[string]bool{"item-g-2": true, "item-adopted-1": true}}
	w.sweep()
	w.kept(itemEntries("g", 1)...)
	w.kept(itemEntries("g", 2)...)
	w.kept("adopted")
	w.gone(itemEntries("g", 3)...)
	w.logged("sweep: " + filepath.Join(w.root, "g", "1") + " left, item 1 merged but its session 1.session is alive")
	w.logged("left, item 2 merged but a session on this node runs it")
}

func TestWorkSweepKeepsAndLogsWhatThePlatformDoesNotKnow(t *testing.T) {
	w := newSweepWorld(t)
	w.file(filepath.Join(w.root, "stranger", "1", "x"), "x")
	w.goal("g", "running")
	w.entries("g", 7)
	w.sweep()
	w.kept("stranger/1", "g/7", "g/7.session", "g/7.attestation.json")
	w.logged("sweep: " + filepath.Join(w.root, "stranger") + " left, unknown to the platform")
	w.logged("sweep: " + filepath.Join(w.root, "g", "7") + " left, unknown to the platform")
}

func TestWorkSweepAGatewayErrorKeepsEveryEntryAndLogsUnread(t *testing.T) {
	w := newSweepWorld(t)
	w.goal("g", "done")
	w.item("g", 1, "merged", "")
	w.gw.goalsErr["org-1"] = errors.New("502 bad gateway")
	if p := w.sweep(); len(p.Removals) != 0 {
		t.Fatalf("removals %v", p.Removals)
	}
	w.kept("g/1")
	w.logged("sweep: org org-1 unread: 502 bad gateway; nothing of it judged")
	w.logged("left, unread")

	w = newSweepWorld(t)
	w.goal("g", "running")
	w.item("g", 1, "merged", "")
	w.gw.itemsErr["goal-g"] = errors.New("timeout")
	if p := w.sweep(); len(p.Removals) != 0 {
		t.Fatalf("removals %v", p.Removals)
	}
	w.kept(itemEntries("g", 1)...)
	w.logged("left, goal g's items unread: timeout")
}

func TestWorkSweepNeverFollowsASymlinkOutOfTheRoot(t *testing.T) {
	w := newSweepWorld(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "precious")
	w.file(filepath.Join(target, "keep.txt"), "x")
	w.goal("g", "running")
	w.item("g", 1, "merged", "")
	w.goal("linked", "done")
	// a goal dir that is a symlink, an item entry that is one, and one
	// inside a checkout the sweep removes
	for _, l := range []string{filepath.Join(w.root, "linked"), filepath.Join(w.root, "g", "2"), filepath.Join(w.root, "g", "1", "out")} {
		if err := os.Symlink(target, l); err != nil {
			t.Fatal(err)
		}
	}
	w.gw.items["goal-g"] = append(w.gw.items["goal-g"], gateway.WorkItem{ID: "item-g-2", Seq: 2, Status: "merged"})
	w.sweep()
	w.gone(itemEntries("g", 1)...)
	w.kept("linked", "g/2")
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Fatalf("symlink's target touched: %v", err)
	}
	w.logged("sweep: " + filepath.Join(w.root, "linked") + " left, a symlink (not followed)")
	w.logged("sweep: " + filepath.Join(w.root, "g", "2") + " left, a symlink (not followed)")
}

func TestWorkSweepLeavesResearchClaimsAndTheNodeSlugDirectory(t *testing.T) {
	w := newSweepWorld(t)
	w.file(filepath.Join(ResearchDir(w.root, "proj", "nightly"), "x"), "x")
	w.file(filepath.Join(ClaimsDir(w.root), "plan-goal-x.json"), "{}")
	w.file(filepath.Join(w.root, "dl360", ".mcp.json"), "{}")
	w.live = LiveSessions{Keep: []string{"dl360"}}
	if p := w.sweep(); len(p.Removals) != 0 {
		t.Fatalf("removals %v", p.Removals)
	}
	w.kept("proj/research/nightly/x", ".claims/plan-goal-x.json", "dl360/.mcp.json")
	for _, l := range w.logs {
		if strings.Contains(l, " left,") {
			t.Errorf("logged %q; research, claims and the node slug are skipped silently", l)
		}
	}
}

func TestWorkSweepRemovesKeptSessionsPastFourteenDays(t *testing.T) {
	w := newSweepWorld(t)
	mk := func(name string, started time.Time) {
		b, _ := json.Marshal(map[string]any{"kind": "evaluate", "startedAt": started})
		w.file(filepath.Join(w.root, "kept-sessions", name, sessionunit.StateFile), string(b))
	}
	mk("old-1", sweepNow.Add(-15*24*time.Hour))
	mk("young-1", sweepNow.Add(-13*24*time.Hour))
	w.sweep()
	w.gone("kept-sessions/old-1")
	w.kept("kept-sessions/young-1")
	w.logged("sweep: removed kept-sessions/old-1 (kept 15d, past 14d, ")
}

func TestWorkSweepDryRunPrintsExactlyWhatApplyLogsAndRemovesNothing(t *testing.T) {
	w := newSweepWorld(t)
	w.goal("g", "running")
	w.item("g", 1, "merged", "")
	w.item("g", 2, "review", "")
	w.file(filepath.Join(w.root, "g", "plan", "x"), "xyz")
	w.goal("d", "done")
	w.item("d", 1, "merged", "")

	plan := w.plan()
	printed := plan.String()
	w.kept(itemEntries("g", 1)...)
	w.kept("g/plan", "d")

	w.logs = nil
	again := w.plan()
	w.logs = nil
	ApplyWorkSweep(again, w.logf)
	if got := strings.Join(w.logs, "\n") + "\n"; got != printed {
		t.Fatalf("dry run printed\n%s\napply logged\n%s", printed, got)
	}
	if len(plan.Removals) != 5 {
		t.Fatalf("removals %d, want 5: %v", len(plan.Removals), plan.Removals)
	}
	w.gone(itemEntries("g", 1)...)
	w.gone("g/plan", "d")
	w.kept(itemEntries("g", 2)...)
}

func TestWorkSweepKeepSessionOutputIsFourteenDays(t *testing.T) {
	if harness.KeepSessionOutput != 14*24*time.Hour {
		t.Fatalf("KeepSessionOutput %v, want 14 days", harness.KeepSessionOutput)
	}
}
