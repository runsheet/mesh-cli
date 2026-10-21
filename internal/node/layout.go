package node

// The node's directories (ADR-039, docs/adr/adr-039-a-session-works-in-its-own-job-directory.md,
// superseding ADR-034's per-org roots). Every unit of work a node holds
// — an item, a goal's plan, a research direction, a merge batch — has
// its own job directory, ~/.mesh/jobs/<job-id>/, named by a job.json
// that says whose it is (its connection, runsheet ADR 066: a tenant and
// a product): its clones, its package store, its scratch, its kept
// streams. Nothing in it is shared with another job, so no job can read
// or poison another's; what jobs share is the node's content-addressed
// cache, which the node fills and no pod mounts. No path carries an
// org's or a product's name; every key is an id the platform gave.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

const (
	// JobsDirName is the jobs tree under ~/.mesh.
	JobsDirName = "jobs"
	// JobFile names a job directory's owner.
	JobFile = "job.json"
)

// Units of work a job directory holds.
const (
	JobItem     = "item"
	JobPlan     = "plan"
	JobResearch = "research"
	JobBatch    = "batch"
)

// Layout is the node's jobs tree under Dir (~/.mesh).
type Layout struct {
	Dir string
}

// DefaultLayout is the Layout under ~/.mesh.
func DefaultLayout() (*Layout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Layout{Dir: filepath.Join(home, ".mesh")}, nil
}

// JobsDir is the parent of every job directory.
func (l *Layout) JobsDir() string { return filepath.Join(l.Dir, JobsDirName) }

// ValidateKey refuses a job key that is not one path
// segment: empty, "." or "..", or carrying a path separator, a NUL or
// surrounding space — any of which would put its directory outside its
// tree or on another's.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return errors.New("layout: empty key")
	case key == "." || key == "..":
		return fmt.Errorf("layout: key %q is not a directory name", key)
	case strings.ContainsAny(key, `/\`+"\x00"):
		return fmt.Errorf("layout: key %q holds a path separator", key)
	case strings.TrimSpace(key) != key:
		return fmt.Errorf("layout: key %q has surrounding space", key)
	}
	return nil
}

// JobDirs are one job's directories.
type JobDirs struct {
	// ID is the job's key: the item's, goal's, direction's or batch's id.
	ID string
	// Dir is the job's own directory, ~/.mesh/jobs/<id>.
	Dir string
	// Scratch is where a host run's TMPDIR is made (MakeScratch).
	Scratch string
	// Kept is where its sessions' streams are kept (harness kept-sessions).
	Kept string
	// Resume holds a build's resume record (ResumeStore).
	Resume string
	// MCP holds its session token files and proxy sockets.
	MCP string
	// Homes is where a sandboxed session's HOME is made.
	Homes string
	// Store is the job's own pnpm store and npm cache (PackageStores).
	Store string
}

// Job are the directories of the job keyed id.
func (l *Layout) Job(id string) (JobDirs, error) {
	if err := ValidateKey(id); err != nil {
		return JobDirs{}, err
	}
	dir := filepath.Join(l.JobsDir(), id)
	return JobDirs{
		ID:      id,
		Dir:     dir,
		Scratch: filepath.Join(dir, ScratchDirName),
		Kept:    filepath.Join(dir, "kept-sessions"),
		Resume:  filepath.Join(dir, "resume"),
		MCP:     filepath.Join(dir, "mcp"),
		Homes:   filepath.Join(dir, "homes"),
		Store:   filepath.Join(dir, "store"),
	}, nil
}

// Checkout is an item's build checkout, a clone of its own, which every
// build attempt, its validation and a resume share.
func (j JobDirs) Checkout() string { return filepath.Join(j.Dir, "checkout") }

// Eval is an item's evaluation's own clone.
func (j JobDirs) Eval() string { return filepath.Join(j.Dir, "eval") }

// Rebase is an item's rebase's own clone.
func (j JobDirs) Rebase() string { return filepath.Join(j.Dir, "rebase") }

// Plan is a goal's planner's directory.
func (j JobDirs) Plan() string { return filepath.Join(j.Dir, "plan") }

// Research is a direction's research directory.
func (j JobDirs) Research() string { return filepath.Join(j.Dir, "research") }

// Attempt is a batch's or main validation's checkout for one attempt:
// <job>/<what>-<attempt>.
func (j JobDirs) Attempt(what string, attempt int) string {
	return filepath.Join(j.Dir, fmt.Sprintf("%s-%d", what, attempt))
}

// connectionOf is the scope of the served org whose id is orgID, which
// names its connection. An org the node does not serve, or a scope the
// platform named no connection for, is refused: a job is opened only
// for a connection the node has.
func connectionOf(scopes Scopes, orgID string) (Scope, error) {
	sc, ok := scopes.ByOrg(orgID)
	if !ok {
		return Scope{}, fmt.Errorf("layout: org id %q is not served by this node", orgID)
	}
	if sc.ConnectionID == "" {
		return sc, fmt.Errorf("layout: org %s has no connection on this node's answer", sc.Name())
	}
	return sc, nil
}

// JobMeta is a job directory's job.json: whose the job is.
type JobMeta struct {
	ID string `json:"id"`
	// Unit is what the job is for: item, plan, research or batch.
	Unit         string `json:"unit"`
	ConnectionID string `json:"connection"`
	OrgID        string `json:"orgId"`
	Product      string `json:"product,omitempty"`
	ItemID       string `json:"itemId,omitempty"`
	GoalID       string `json:"goalId,omitempty"`
	DirectionID  string `json:"directionId,omitempty"`
	BatchID      string `json:"batchId,omitempty"`
	// ThreadID is the thread that started the job, if one did (ADR-039,
	// A thread starts jobs).
	ThreadID string `json:"threadId,omitempty"`
	// Label is how the log names it: goal/seq, a goal's slug, a
	// direction's project/slug.
	Label   string    `json:"label,omitempty"`
	Started time.Time `json:"started"`
}

// ReadJob reads dir's job.json.
func ReadJob(dir string) (JobMeta, error) {
	b, err := os.ReadFile(filepath.Join(dir, JobFile))
	if err != nil {
		return JobMeta{}, err
	}
	var m JobMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return JobMeta{}, fmt.Errorf("%s: %w", filepath.Join(dir, JobFile), err)
	}
	return m, nil
}

// OpenJob makes the job directory of m (keyed m.ID) and its job.json
// before anything is checked out in it. A job.json already there is
// kept as it is — the job is the same work, opened again — unless it
// names another connection, which is refused: a job's files never move
// between connections.
func (l *Layout) OpenJob(m JobMeta) (JobDirs, error) {
	j, err := l.Job(m.ID)
	if err != nil {
		return JobDirs{}, err
	}
	if old, err := ReadJob(j.Dir); err == nil {
		if old.ConnectionID != m.ConnectionID {
			return JobDirs{}, fmt.Errorf("layout: job %s is connection %s's, not %s", m.ID, old.ConnectionID, m.ConnectionID)
		}
		return j, nil
	}
	if err := os.MkdirAll(j.Dir, 0o755); err != nil {
		return JobDirs{}, err
	}
	if m.Started.IsZero() {
		m.Started = time.Now().UTC()
	}
	return j, writeJSONAtomic(filepath.Join(j.Dir, JobFile), m)
}

// writeJSONAtomic writes v to path through a temporary file renamed over
// it.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// FoundJob is a job directory and its job.json.
type FoundJob struct {
	Dir  string
	Meta JobMeta
}

// Jobs lists every job directory with a readable job.json, oldest first.
// A directory without one is not a job and is left out; no jobs dir is
// none.
func (l *Layout) Jobs() ([]FoundJob, error) {
	entries, err := os.ReadDir(l.JobsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []FoundJob
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(l.JobsDir(), e.Name())
		m, err := ReadJob(dir)
		if err != nil || m.ID != e.Name() {
			continue
		}
		out = append(out, FoundJob{Dir: dir, Meta: m})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Meta.Started.Before(out[j].Meta.Started) })
	return out, nil
}

// jobOf is the job directory dir is in (dir itself or anything under
// it) and its job.json, else false.
func jobOf(dir string) (string, JobMeta, bool) {
	dir = filepath.Clean(dir)
	for p := dir; ; {
		parent := filepath.Dir(p)
		if parent == p {
			return "", JobMeta{}, false
		}
		if filepath.Base(parent) == JobsDirName {
			if m, err := ReadJob(p); err == nil && m.ID == filepath.Base(p) {
				return p, m, true
			}
			return "", JobMeta{}, false
		}
		p = parent
	}
}

// SweepKeptSessions runs harness.SweepKeptSessions over every job's
// kept sessions, each job's own only. It goes on past a job it cannot
// read and returns the errors joined.
func (l *Layout) SweepKeptSessions(now time.Time) error {
	if l == nil {
		return nil
	}
	jobs, err := l.Jobs()
	if err != nil {
		return err
	}
	var errs []error
	for _, fj := range jobs {
		j, err := l.Job(fj.Meta.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := harness.SweepKeptSessions(j.Kept, now); err != nil {
			errs = append(errs, fmt.Errorf("kept sessions of job %s: %w", fj.Meta.ID, err))
		}
	}
	return errors.Join(errs...)
}

// ScratchRoots are every job's scratch root, for the start-up scratch
// sweep.
func (l *Layout) ScratchRoots() []string {
	jobs, _ := l.Jobs()
	out := make([]string, 0, len(jobs))
	for _, fj := range jobs {
		if j, err := l.Job(fj.Meta.ID); err == nil {
			out = append(out, j.Scratch)
		}
	}
	return out
}

// ConnectionsOf are the connection ids the scopes name, for the sweep's
// connection-gone rule; nil — no job judged by its connection — when
// there are no scopes or any scope names none (a platform before ADR
// 066 answered no connection).
func ConnectionsOf(scopes Scopes) map[string]bool {
	if len(scopes) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, sc := range scopes {
		if sc.ConnectionID == "" {
			return nil
		}
		out[sc.ConnectionID] = true
	}
	return out
}
