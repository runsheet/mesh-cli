package node

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// BatchJobKeep is how long a merge batch's or main validation's job is
// kept once nothing in it runs: its attempts remove their own checkouts,
// so what is left is job.json and the kept streams.
const BatchJobKeep = 6 * time.Hour

// PlanJobSweep judges every job directory under l by the platform's
// answer (ADR-039; the rules of mesh-cli ADR 008's work sweep) and
// returns what nothing needs:
//
//   - an item's job once the item is merged, cancelled or superseded, or
//     failed for over FailedWorkspaceKeep;
//   - a plan's job once its goal is not planning;
//   - a batch's or a thread turn's job BatchJobKeep after it was opened;
//   - any job whose connection the node no longer has (connections,
//     when known: the connection ids of the node's answer), its work
//     released by the platform on the revocation;
//   - each job's kept sessions past harness.KeepSessionOutput.
//
// A research job stays: its checkouts are fetched over for the next
// research of the direction. Nothing a live session needs (its subject
// in live.IDs, or a session dir in the job whose session is alive), no
// symlink and nothing outside the jobs tree is ever in it; a directory
// without a job.json is left and logged. It removes nothing.
func PlanJobSweep(ctx context.Context, gw Gateway, l *Layout, connections map[string]bool, live LiveSessions, now time.Time, logf func(string, ...any)) (WorkSweepPlan, error) {
	root := l.JobsDir()
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
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(root, name)
		if strings.HasPrefix(name, ".") { // .claims
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			logf("sweep: %s left, a symlink (not followed)", path)
			continue
		}
		if !e.IsDir() {
			continue
		}
		m, err := ReadJob(path)
		if err != nil || m.ID != name {
			logf("sweep: %s left, no job.json naming it", path)
			continue
		}
		s.job(l, m, path, connections)
	}
	return plan, nil
}

// job judges one job directory.
func (s *workSweep) job(l *Layout, m JobMeta, dir string, connections map[string]bool) {
	label := cmpLabel(m)
	sessions := s.sessionsIn(dir)
	alive := s.jobLive(m, sessions)
	if connections != nil && !connections[m.ConnectionID] {
		if alive != "" {
			s.logf("sweep: job %s left, its connection is gone but %s", label, alive)
			return
		}
		s.addJob(label, dir, "connection "+m.ConnectionID+" gone")
		return
	}
	why := ""
	switch m.Unit {
	case JobItem:
		it, err := s.gw.WorkItem(s.ctx, m.ItemID)
		if err != nil || it == nil {
			s.logf("sweep: job %s left, item unread: %v", label, err)
			return
		}
		why = s.itemGone(*it)
		if why == "" && it.Status == "failed" && statusAt(*it).IsZero() {
			s.logf("sweep: job %s left, item failed with no statusAt (an older platform)", label)
		}
	case JobPlan:
		g, err := s.gw.Goal(s.ctx, m.GoalID)
		if err != nil || g == nil {
			s.logf("sweep: job %s left, goal unread: %v", label, err)
			return
		}
		if g.Status != "planning" {
			why = "goal " + g.Status + ", not planning"
		}
	case JobBatch, JobThread:
		// a turn's job is its session's scratch and streams; the thread
		// itself is the product's
		if age := s.now.Sub(m.Started); age > BatchJobKeep {
			why = fmt.Sprintf("opened %s ago", days(age))
		}
	case JobResearch:
	default:
		s.logf("sweep: job %s left, a unit the sweep does not know (%q)", label, m.Unit)
		return
	}
	if why == "" {
		if j, err := l.Job(m.ID); err == nil {
			s.keptSessionsOf(label, j.Kept)
		}
		return
	}
	if alive != "" {
		s.logf("sweep: job %s left, %s but %s", label, why, alive)
		return
	}
	s.addJob(label, dir, why)
}

// cmpLabel is how the sweep names a job: its label, else its id.
func cmpLabel(m JobMeta) string {
	if m.Label != "" {
		return m.Label
	}
	return m.ID
}

// addJob puts a whole job directory in the plan.
func (s *workSweep) addJob(label, dir, why string) {
	s.add(label, "", dir, why)
}

// sessionsIn are the session dirs directly in a job's directory, read.
func (s *workSweep) sessionsIn(dir string) []sessionAt {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []sessionAt
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".session") || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir, e.Name())
		st, err := sessionunit.ReadState(path)
		if err != nil {
			continue
		}
		out = append(out, sessionAt{name: e.Name(), state: st, alive: s.alive(st, path)})
	}
	return out
}

// jobLive is why a job is still needed: its subject in the live set, or
// a session in it alive or named by the live set.
func (s *workSweep) jobLive(m JobMeta, sessions []sessionAt) string {
	for _, id := range []string{m.ID, m.ItemID, m.GoalID, m.DirectionID, m.BatchID} {
		if id != "" && s.live.IDs[id] {
			return "a session on this node runs it"
		}
	}
	for _, se := range sessions {
		if se.alive || s.live.IDs[se.state.ItemID] || s.live.IDs[se.state.GoalID] {
			return "its session " + se.name + " is alive"
		}
	}
	return ""
}

// keptSessionsOf plans each of a job's kept sessions older than
// harness.KeepSessionOutput, named under the job's label.
func (s *workSweep) keptSessionsOf(label, dir string) {
	before := len(s.plan.Removals)
	s.keptSessions(dir)
	for i := before; i < len(s.plan.Removals); i++ {
		s.plan.Removals[i].Goal = label + "/" + keptSessionsName
	}
}
