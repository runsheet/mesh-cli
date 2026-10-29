package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// Threads on the node (projectmesh-io/api ADR 001, Threads, and its
// "threads" amendment; Mesh v1 PR 4.2). A thread job's turns reach its
// live session as one fixed line each (turnLine). After the kind's
// park_after with no turn in flight the session's input closes and the
// lease is handed back with cause parked and the session's id as its
// session_ref. Claude keeps a session under the directory it ran in, so
// a thread's workspace is keyed by its job, not its lease —
// <work-root>/threads/<job>/work — and a park keeps it, with a marker
// (parkedFile) naming the session. The job's next lease on this node
// (Mesh offers it here first, for two minutes) finds the marker, points
// the checkout's origin at the new lease's relay grant and resumes the
// session with `claude -p --resume`, its first message the newest turn.
// A lease on any other node starts a fresh session, which reads the
// thread's history with the product's brief tool. Any end but a park
// removes the workspace, and a parked one older than threadKeep is swept.

// threadKeep is how long a parked thread's workspace waits for its
// job's next lease on this node.
const threadKeep = 24 * time.Hour

// parkedFile is the marker a park leaves in the thread's directory.
const parkedFile = "parked.json"

// parked is the marker: the session parked and when.
type parked struct {
	JobID      string    `json:"job_id"`
	LeaseID    string    `json:"lease_id"`
	SessionRef string    `json:"session_ref"`
	ParkedAt   time.Time `json:"parked_at"`
}

var unsafeDirName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// threadDir is a thread job's directory on this node.
func (r *Runtime) threadDir(jobID string) string {
	return filepath.Join(r.WorkRoot, "threads", unsafeDirName.ReplaceAllString(jobID, "_"))
}

// turnToolFor is the brief's turn tool as the session sees it,
// mcp__<server>__<tool>, on the product's server (the brief's first,
// else the product's slug); "" when the kind names none.
func turnToolFor(b *meshapi.Brief) string {
	tool := strings.TrimSpace(b.TurnTool)
	if tool == "" || strings.HasPrefix(tool, "mcp__") {
		return tool
	}
	server := b.Product
	if len(b.Servers) > 0 && b.Servers[0].Name != "" {
		server = b.Servers[0].Name
	}
	return "mcp__" + server + "__" + tool
}

// threadWorkspace is a thread lease's workspace and the session it
// resumes: the parked session the marker names when it is the brief's
// session_ref and its checkout is still there, else "" over a fresh,
// empty workspace. The marker is removed either way: the lease now owns
// the directory.
func (r *Runtime) threadWorkspace(b *meshapi.Brief) (workspace, resume string, err error) {
	dir := r.threadDir(b.JobID)
	workspace = filepath.Join(dir, "work")
	m, ok := readParked(dir)
	if ok && b.SessionRef != "" && m.SessionRef == b.SessionRef {
		if _, err := os.Stat(workspace); err == nil {
			resume = m.SessionRef
		}
	}
	if resume == "" {
		if err := os.RemoveAll(dir); err != nil {
			return "", "", fmt.Errorf("the thread's old directory: %w", err)
		}
	}
	_ = os.Remove(filepath.Join(dir, parkedFile))
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return "", "", fmt.Errorf("the thread's directory: %w", err)
	}
	return workspace, resume, nil
}

// settleThread keeps a parked thread's workspace with its marker, or
// removes the thread's directory on any other end; then sweeps the
// parked workspaces past threadKeep.
func (r *Runtime) settleThread(b *meshapi.Brief, sessionRef string, isParked bool) {
	if b.Thread == nil {
		return
	}
	dir := r.threadDir(b.JobID)
	if isParked && sessionRef != "" {
		raw, _ := json.Marshal(parked{JobID: b.JobID, LeaseID: b.LeaseID, SessionRef: sessionRef, ParkedAt: time.Now().UTC()})
		if err := os.WriteFile(filepath.Join(dir, parkedFile), raw, 0o600); err != nil {
			r.logf("nodev1: lease %s: keeping the parked thread: %v", b.LeaseID, err)
			_ = os.RemoveAll(dir)
		}
	} else if err := os.RemoveAll(dir); err != nil {
		r.logf("nodev1: lease %s: removing the thread's directory: %v", b.LeaseID, err)
	}
	r.sweepThreads(time.Now())
}

// sweepThreads removes each thread directory whose park is older than
// threadKeep, and each with no marker (a lease that never settled: the
// node stopped under it) once it is that old, unless a lease of the
// node runs its job now.
func (r *Runtime) sweepThreads(now time.Time) {
	root := filepath.Join(r.WorkRoot, "threads")
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	live := r.liveJobs()
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		at := time.Time{}
		if m, ok := readParked(dir); ok {
			at = m.ParkedAt
		} else if info, err := e.Info(); err == nil {
			if live[e.Name()] {
				continue
			}
			at = info.ModTime()
		}
		if now.Sub(at) < threadKeep {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			r.logf("nodev1: sweeping the parked thread %s: %v", e.Name(), err)
			continue
		}
		r.logf("nodev1: swept the thread workspace %s, parked since %s", e.Name(), at.Format(time.RFC3339))
	}
}

// liveJobs are the thread directory names of the jobs the node's leases
// run now.
func (r *Runtime) liveJobs() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for _, e := range r.leases.All() {
		if e.Value.jobID != "" {
			out[unsafeDirName.ReplaceAllString(e.Value.jobID, "_")] = true
		}
	}
	return out
}

// readParked reads a thread directory's marker.
func readParked(dir string) (parked, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, parkedFile))
	if err != nil {
		return parked{}, false
	}
	var m parked
	if json.Unmarshal(raw, &m) != nil || m.SessionRef == "" {
		return parked{}, false
	}
	return m, true
}

// reattach points a resumed thread's checkout at the new lease's grant:
// origin is the grant's capability URL, as checkout sets it, and nothing
// is cloned again.
func reattach(ctx context.Context, grant *gitrelay.Grant, b *meshapi.Brief, workspace string) error {
	if _, err := os.Stat(filepath.Join(workspace, ".git")); err != nil {
		return errors.New("the parked thread's checkout is gone")
	}
	cmd := exec.CommandContext(ctx, "git", append(append([]string(nil), noCredentials...), "remote", "set-url", "origin", grant.Base()+b.Git.Repo+".git")...)
	cmd.Dir = workspace
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git remote set-url: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// threadPrompt is a thread lease's first message: for a resumed session
// the newest turn's line alone (the session has the rest); for a fresh
// one, prompt then that line, when the thread has a turn.
func threadPrompt(b *meshapi.Brief, prompt, resume string) string {
	var line string
	if t := b.LastTurn; t != nil && t.TurnID != "" {
		line = turnLine(turnToolFor(b), t.TurnID, t.Seq)
	}
	switch {
	case resume != "" && line != "":
		return line
	case resume != "":
		return "The thread resumed on this node with no new turn. Carry on where you left off."
	case line != "":
		return prompt + "\n\n" + line + "\n"
	}
	return prompt
}
