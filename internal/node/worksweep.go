package node

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// FailedWorkspaceKeep is how long a failed item's checkout is kept after
// its status last changed (WorkItem.StatusAt): a week, for a person to
// look into it.
const FailedWorkspaceKeep = 7 * 24 * time.Hour

// keptSessionsName is the work root's kept-sessions dir
// (harness.KeepSessionOutput).
const keptSessionsName = "kept-sessions"

// LiveSessions is what the work sweep's caller knows runs on this node:
// IDs the item and goal ids of every session it runs or has adopted
// (the loop's RunningIDs and Adopted subjects), Keep the top-level
// names under the work root that are never swept (the channel's
// <node-slug>), and Sandbox the node's sandbox, through which a
// sandboxed session's container is read.
type LiveSessions struct {
	IDs     map[string]bool
	Keep    []string
	Sandbox *sandbox.Sandbox
}

// WorkRemoval is one entry the work sweep removes: Goal the goal slug
// (or kept-sessions), Entry the name under it — a seq, `<seq>.session`,
// `<seq>.attestation.json`, plan, a kept session's name, or "" for the
// goal's whole directory — Why the platform's answer, Bytes its size.
type WorkRemoval struct {
	Goal  string
	Entry string
	Path  string
	Why   string
	Bytes int64
}

// Name is `<goal>/<entry>`, or the goal alone for its whole directory.
func (r WorkRemoval) Name() string {
	if r.Entry == "" {
		return r.Goal
	}
	return r.Goal + "/" + r.Entry
}

// Line is the log line Apply writes for the removal.
func (r WorkRemoval) Line() string {
	return fmt.Sprintf("sweep: removed %s (%s, %s)", r.Name(), r.Why, FormatBytes(r.Bytes))
}

// WorkSweepPlan is what one pass of the work sweep would remove.
type WorkSweepPlan struct {
	Root     string
	Removals []WorkRemoval
}

// Bytes is the plan's total size.
func (p WorkSweepPlan) Bytes() int64 {
	var n int64
	for _, r := range p.Removals {
		n += r.Bytes
	}
	return n
}

// totalLine is the pass's last line.
func totalLine(n int, bytes int64) string {
	return fmt.Sprintf("sweep: removed %d entries, %s freed", n, FormatBytes(bytes))
}

// Lines are the lines ApplyWorkSweep logs when every removal goes
// through: one per removal, then the total.
func (p WorkSweepPlan) Lines() []string {
	lines := make([]string, 0, len(p.Removals)+1)
	for _, r := range p.Removals {
		lines = append(lines, r.Line())
	}
	return append(lines, totalLine(len(p.Removals), p.Bytes()))
}

// String is the dry run: Lines, one per line.
func (p WorkSweepPlan) String() string { return strings.Join(p.Lines(), "\n") + "\n" }

// ApplyWorkSweep removes each of the plan's entries, logging one line
// per removal and the total; a removal that fails is logged and left
// for the next pass. It returns how many went and what they freed.
func ApplyWorkSweep(plan WorkSweepPlan, logf func(string, ...any)) (removed int, freed int64) {
	for _, r := range plan.Removals {
		if err := RemoveScratch(r.Path); err != nil {
			logf("sweep: removing %s: %v; left for the next pass", r.Name(), err)
			continue
		}
		logf("%s", r.Line())
		removed++
		freed += r.Bytes
	}
	logf("%s", totalLine(removed, freed))
	return removed, freed
}

// PlanWorkSweep judges every entry under the work root by the
// platform's answer (mesh-cli ADR 008, The work sweep) and returns what
// nothing needs: an item's checkout, its session dir and its
// attestation copy once the item is merged, cancelled or superseded, or
// failed for over FailedWorkspaceKeep; a goal's whole directory once it
// is done or cancelled; its plan/ once it is not planning; a kept
// session older than harness.KeepSessionOutput. Nothing a live session
// needs, nothing the platform does not know or cannot answer for, no
// symlink and nothing outside root is ever in it. It removes nothing.
func PlanWorkSweep(ctx context.Context, gw Gateway, root string, orgIDs []string, live LiveSessions, now time.Time, logf func(string, ...any)) (WorkSweepPlan, error) {
	plan := WorkSweepPlan{Root: root}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return plan, nil
		}
		return plan, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return plan, err
	}
	s := &workSweep{ctx: ctx, gw: gw, root: root, realRoot: realRoot, live: live, now: now, logf: logf, plan: &plan}

	bySlug := map[string][]gateway.Goal{}
	unread := false
	for _, org := range orgIDs {
		goals, err := gw.Goals(ctx, org)
		if err != nil {
			logf("sweep: org %s unread: %v; nothing of it judged", org, err)
			unread = true
			continue
		}
		for _, g := range goals {
			bySlug[g.Slug] = append(bySlug[g.Slug], g)
		}
	}

	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(root, name)
		switch {
		case strings.HasPrefix(name, "."), // .claims, .scratch, .locks, .plans
			name == "merge-batches", name == "main-validations",
			slices.Contains(live.Keep, name):
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			logf("sweep: %s left, a symlink (not followed)", path)
			continue
		}
		if !info.IsDir() {
			continue
		}
		if name == keptSessionsName {
			s.keptSessions(path)
			continue
		}
		goals := bySlug[name]
		switch {
		case len(goals) == 1:
			s.goal(goals[0], path)
		case len(goals) > 1:
			logf("sweep: %s left, %d served goals have the slug %s", path, len(goals), name)
		case isDir(filepath.Join(path, "research")):
			continue // <project-slug>/research/<direction>: ResearchDir's
		case unread:
			logf("sweep: %s left, unread (an org's goals could not be read)", path)
		default:
			logf("sweep: %s left, unknown to the platform", path)
		}
	}
	return plan, nil
}

// workSweep is one pass's judging.
type workSweep struct {
	ctx            context.Context
	gw             Gateway
	root, realRoot string
	live           LiveSessions
	now            time.Time
	logf           func(string, ...any)
	plan           *WorkSweepPlan
}

// add puts path in the plan, once it resolves under the root and is no
// symlink.
func (s *workSweep) add(goal, entry, path, why string) {
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		s.logf("sweep: %s left, a symlink (not followed)", path)
		return
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || !underWorkRoot(s.realRoot, filepath.Join(parent, filepath.Base(path))) {
		s.logf("sweep: %s left, not under the work root %s", path, s.root)
		return
	}
	s.plan.Removals = append(s.plan.Removals, WorkRemoval{Goal: goal, Entry: entry, Path: path, Why: why, Bytes: treeBytes(path)})
}

// sessionAt is one session dir in a goal's directory, read.
type sessionAt struct {
	name  string // its entry name, `<base>.session`
	state sessionunit.State
	alive bool
}

// goal judges one served goal's directory.
func (s *workSweep) goal(g gateway.Goal, dir string) {
	items, err := s.gw.GoalWorkItems(s.ctx, g.ID)
	if err != nil {
		s.logf("sweep: %s left, goal %s's items unread: %v", dir, g.Slug, err)
		return
	}
	bySeq := map[int]gateway.WorkItem{}
	for _, it := range items {
		bySeq[it.Seq] = it
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.logf("sweep: %s left, unreadable: %v", dir, err)
		return
	}
	var sessions []sessionAt
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".session") || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		st, err := sessionunit.ReadState(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		sessions = append(sessions, sessionAt{name: e.Name(), state: st, alive: s.alive(st, filepath.Join(dir, e.Name()))})
	}

	if g.Status == "done" || g.Status == "cancelled" {
		if why := s.goalLive(g, items, sessions); why != "" {
			s.logf("sweep: %s left, goal %s but %s", dir, g.Status, why)
			return
		}
		s.add(g.Slug, "", dir, "goal "+g.Status)
		return
	}

	noStatusAt := map[int]bool{}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(dir, name)
		if e.Type()&fs.ModeSymlink != 0 {
			s.logf("sweep: %s left, a symlink (not followed)", path)
			continue
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".session"), ".attestation.json")
		if base == "plan" {
			if g.Status == "planning" {
				continue
			}
			if why := s.planLive(g, sessions); why != "" {
				s.logf("sweep: %s left, %s", path, why)
				continue
			}
			s.add(g.Slug, name, path, "goal "+g.Status+", not planning")
			continue
		}
		seqText := strings.TrimSuffix(strings.TrimSuffix(base, "-eval"), "-rebase")
		seq, err := strconv.Atoi(seqText)
		if err != nil || seq < 0 || strconv.Itoa(seq) != seqText {
			s.logf("sweep: %s left, not a layout the sweep knows", path)
			continue
		}
		if seq == 0 {
			continue // the goal doc's checkout (RecordEndedGoal): goes with the goal
		}
		it, ok := bySeq[seq]
		if !ok {
			s.logf("sweep: %s left, unknown to the platform", path)
			continue
		}
		why := s.itemGone(it)
		if why == "" {
			if it.Status == "failed" && !noStatusAt[seq] && statusAt(it).IsZero() {
				noStatusAt[seq] = true
				s.logf("sweep: %s/%d left, item failed with no statusAt (an older platform)", g.Slug, seq)
			}
			continue
		}
		if live := s.itemLive(it, base, sessions); live != "" {
			s.logf("sweep: %s left, item %d %s but %s", path, seq, it.Status, live)
			continue
		}
		s.add(g.Slug, name, path, why)
	}
}

// itemGone is why the item's entries go, "" when they stay: merged,
// cancelled or superseded, or failed for over FailedWorkspaceKeep.
func (s *workSweep) itemGone(it gateway.WorkItem) string {
	switch it.Status {
	case "merged", "cancelled", "superseded":
		return fmt.Sprintf("item %d %s", it.Seq, it.Status)
	case "failed":
		at := statusAt(it)
		if at.IsZero() {
			return ""
		}
		if age := s.now.Sub(at); age > FailedWorkspaceKeep {
			return fmt.Sprintf("item %d failed %s ago", it.Seq, days(age))
		}
	}
	return ""
}

// statusAt is the item's StatusAt, zero when empty or unreadable.
func statusAt(it gateway.WorkItem) time.Time {
	t, err := time.Parse(time.RFC3339, it.StatusAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// alive is whether a session dir's session still runs: its pid (or a
// sandboxed one's container) alive. A sandboxed session on a node with
// no sandbox to ask counts as alive: it cannot be told otherwise.
func (s *workSweep) alive(st sessionunit.State, dir string) bool {
	f := FoundSession{Dir: dir, State: st, Sandbox: s.live.Sandbox}
	if f.Sandboxed() && s.live.Sandbox == nil {
		return true
	}
	return f.Alive()
}

// itemLive is why the item's entries are needed, "" when nothing needs
// them: its id in the live set, or a session of it — the one beside the
// entry, or any naming the item — alive.
func (s *workSweep) itemLive(it gateway.WorkItem, base string, sessions []sessionAt) string {
	if s.live.IDs[it.ID] {
		return "a session on this node runs it"
	}
	for _, se := range sessions {
		if !se.alive {
			continue
		}
		if se.name == base+".session" || se.state.ItemID == it.ID {
			return "its session " + se.name + " is alive"
		}
	}
	return ""
}

// planLive is why the goal's plan/ is needed, "" when nothing needs it.
func (s *workSweep) planLive(g gateway.Goal, sessions []sessionAt) string {
	if s.live.IDs[g.ID] {
		return "a session on this node plans the goal"
	}
	for _, se := range sessions {
		if se.alive && (se.name == "plan.session" || (se.state.GoalID == g.ID && se.state.ItemID == "")) {
			return "its session " + se.name + " is alive"
		}
	}
	return ""
}

// goalLive is why a finished goal's directory is still needed: any of
// its subjects in the live set, or any session in it alive.
func (s *workSweep) goalLive(g gateway.Goal, items []gateway.WorkItem, sessions []sessionAt) string {
	if s.live.IDs[g.ID] {
		return "a session on this node runs it"
	}
	for _, it := range items {
		if s.live.IDs[it.ID] {
			return fmt.Sprintf("a session on this node runs item %d", it.Seq)
		}
	}
	for _, se := range sessions {
		if se.alive || s.live.IDs[se.state.ItemID] || s.live.IDs[se.state.GoalID] {
			return "its session " + se.name + " is alive"
		}
	}
	return ""
}

// keptSessions plans each kept session older than
// harness.KeepSessionOutput.
func (s *workSweep) keptSessions(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if age := s.now.Sub(harness.KeptSessionStartedAt(path)); age > harness.KeepSessionOutput {
			s.add(keptSessionsName, e.Name(), path, fmt.Sprintf("kept %s, past %s", days(age), days(harness.KeepSessionOutput)))
		}
	}
}

// days is d in whole days, `8d`; under a day in hours.
func days(d time.Duration) string {
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// treeBytes is the sum of the Lstat sizes under path, no symlink
// followed.
func treeBytes(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

// underWorkRoot is whether path is inside root (root itself is not).
func underWorkRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}

func isDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}
