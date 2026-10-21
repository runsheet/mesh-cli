package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Confinement is the trees a session may mount from (mesh-cli ADR-039):
// Job its job's key, Roots its job's own directory and its connection's
// caches (~/.mesh/jobs/<job> and ~/.mesh/connections/<connection>), and
// Homes where its HOME is made, inside the job. Every mount source — the
// workspace, the HOME, a worktree's git common dir and its admin
// entries, the package stores, the hooks — must resolve, symlinks and
// all, inside one of Roots, or the session is refused. The zero
// Confinement confines nothing: a session outside any job (a person's
// own `mesh` run).
type Confinement struct {
	Job   string
	Roots []string
	Homes string
}

// IsZero says c confines nothing.
func (c Confinement) IsZero() bool { return len(c.Roots) == 0 }

// ErrMountOutsideJob is what every refused mount Is: a mount source that
// resolves outside the session's job and connection.
var ErrMountOutsideJob = errors.New("mount outside the job's roots")

// MountOutsideJobError is a refused mount: What it was (workspace, home,
// git common dir, git dir, store, hooks), Path the source as given,
// Resolved what it resolved to (the same when no symlink moved it, ""
// when it could not be resolved), and the job and its roots. The session
// starts nothing; no other source is mounted in its place.
type MountOutsideJobError struct {
	What     string
	Path     string
	Resolved string
	Job      string
	Roots    []string
	// Err is why Path could not be resolved, else nil.
	Err error
}

func (e *MountOutsideJobError) Error() string {
	roots := strings.Join(e.Roots, ", ")
	switch {
	case e.Err != nil:
		return fmt.Sprintf("sandbox: %s %s of job %s cannot be resolved inside %s: %v; refused", e.What, e.Path, e.Job, roots, e.Err)
	case e.Resolved != "" && e.Resolved != e.Path:
		return fmt.Sprintf("sandbox: %s %s (resolves to %s) is outside job %s's roots %s; refused", e.What, e.Path, e.Resolved, e.Job, roots)
	default:
		return fmt.Sprintf("sandbox: %s %s is outside job %s's roots %s; refused", e.What, e.Path, e.Job, roots)
	}
}

// Is makes every refusal ErrMountOutsideJob.
func (e *MountOutsideJobError) Is(target error) bool { return target == ErrMountOutsideJob }

// Unwrap is the resolution's error, when there was one.
func (e *MountOutsideJobError) Unwrap() error { return e.Err }

// Check refuses path, a mount source that is what, unless it resolves
// (filepath.EvalSymlinks, so a symlink inside a root pointing out of it
// is followed out and refused) inside one of c's resolved roots. A zero
// c checks nothing; a path that cannot be resolved, or a confinement
// none of whose roots can be, is refused.
func (c Confinement) Check(what, path string) error {
	if c.IsZero() {
		return nil
	}
	refuse := func(resolved string, err error) error {
		return &MountOutsideJobError{What: what, Path: path, Resolved: resolved, Job: c.Job, Roots: c.Roots, Err: err}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return refuse("", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return refuse("", err)
	}
	var rootErr error
	for _, r := range c.Roots {
		root, err := filepath.EvalSymlinks(r)
		if err != nil {
			rootErr = err
			continue
		}
		if within(root, resolved) {
			return nil
		}
	}
	if rootErr != nil {
		return refuse(resolved, fmt.Errorf("job root: %w", rootErr))
	}
	return refuse(resolved, nil)
}

// within says path is root or under it; both already resolved.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// WithConfinement is sb for the sessions of one job: the same runtime,
// image, limits, network and stores, its sessions' HOMEs made under the
// job (c.Homes) and every mount checked against c (Session.CheckedMounts).
// A zero c, or a nil sb, is sb.
func (sb *Sandbox) WithConfinement(c Confinement) *Sandbox {
	if sb == nil || c.IsZero() {
		return sb
	}
	cp := *sb
	cp.Confine = c
	if c.Homes != "" {
		cp.TempDir = c.Homes
	}
	return &cp
}

// ensureHomes makes a confined sandbox's HOMEs dir, 0700: a bind mount's
// parent, and nothing another job's session can list.
func (sb *Sandbox) ensureHomes() error {
	if sb.Confine.IsZero() || sb.TempDir == "" {
		return nil
	}
	return os.MkdirAll(sb.TempDir, 0o700)
}

// namedMount is a mount and what it is, for the refusal's words.
type namedMount struct {
	Mount
	what string
}

// CheckedMounts are Mounts(workspace) with every source checked against
// the session's confinement: the first outside it refuses the whole
// list with a *MountOutsideJobError naming the path and the job. An
// unconfined session's are Mounts' as they are.
func (s *Session) CheckedMounts(workspace string) ([]Mount, error) {
	named := s.namedMounts(workspace)
	out := make([]Mount, 0, len(named))
	for _, m := range named {
		if err := s.confine.Check(m.what, m.Source); err != nil {
			return nil, err
		}
		out = append(out, m.Mount)
	}
	return out, nil
}
