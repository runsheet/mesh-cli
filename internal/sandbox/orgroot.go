package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OrgRoot is the one tree a session of a node serving several orgs may
// mount from (mesh-cli ADR-034, Refusals): Org its key, the scope's
// Name(), and Path the org's own tree, ~/.mesh/orgs/<org> (the node's
// OrgRoots.Base). Every mount source — the workspace, the HOME, a
// worktree's git common dir and its admin entries, the package stores,
// the hooks — must resolve, symlinks and all, inside Path, or the
// session is refused. The zero OrgRoot confines nothing: a node serving
// one org keeps today's paths, with no other org to keep it from.
type OrgRoot struct {
	Org  string
	Path string
}

// IsZero says r confines nothing.
func (r OrgRoot) IsZero() bool { return r.Path == "" }

// HomesDir is where a confined session's HOME is made, under the org's
// own tree, so its HOME is inside the root it is checked against.
func (r OrgRoot) HomesDir() string { return filepath.Join(r.Path, "homes") }

// ErrMountOutsideOrg is what every refused mount Is: a mount source that
// resolves outside the session's org root.
var ErrMountOutsideOrg = errors.New("mount outside the org's root")

// MountOutsideOrgError is a refused mount: What it was (workspace, home,
// git common dir, git dir, store, hooks), Path the source as given,
// Resolved what it resolved to (the same when no symlink moved it, ""
// when it could not be resolved), and the org and its root. The session
// starts nothing; no other source is mounted in its place.
type MountOutsideOrgError struct {
	What     string
	Path     string
	Resolved string
	Org      string
	Root     string
	// Err is why Path could not be resolved, else nil.
	Err error
}

func (e *MountOutsideOrgError) Error() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("sandbox: %s %s of org %s cannot be resolved inside %s: %v; refused", e.What, e.Path, e.Org, e.Root, e.Err)
	case e.Resolved != "" && e.Resolved != e.Path:
		return fmt.Sprintf("sandbox: %s %s (resolves to %s) is outside org %s's root %s; refused", e.What, e.Path, e.Resolved, e.Org, e.Root)
	default:
		return fmt.Sprintf("sandbox: %s %s is outside org %s's root %s; refused", e.What, e.Path, e.Org, e.Root)
	}
}

// Is makes every refusal ErrMountOutsideOrg.
func (e *MountOutsideOrgError) Is(target error) bool { return target == ErrMountOutsideOrg }

// Unwrap is the resolution's error, when there was one.
func (e *MountOutsideOrgError) Unwrap() error { return e.Err }

// Check refuses path, a mount source that is what, unless it resolves
// (filepath.EvalSymlinks, so a symlink inside the root pointing out of
// it is followed out and refused) inside r's own resolved root. A zero
// r checks nothing; a root or path that cannot be resolved is refused.
func (r OrgRoot) Check(what, path string) error {
	if r.IsZero() {
		return nil
	}
	refuse := func(resolved string, err error) error {
		return &MountOutsideOrgError{What: what, Path: path, Resolved: resolved, Org: r.Org, Root: r.Path, Err: err}
	}
	root, err := filepath.EvalSymlinks(r.Path)
	if err != nil {
		return refuse("", fmt.Errorf("org root: %w", err))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return refuse("", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return refuse("", err)
	}
	if !within(root, resolved) {
		return refuse(resolved, nil)
	}
	return nil
}

// within says path is root or under it; both already resolved.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// WithOrg is sb for the sessions of org r: the same runtime, image,
// limits, network and stores, its sessions' HOMEs made under r's tree
// (HomesDir) and every mount checked against it (Session.CheckedMounts).
// A zero r, or a nil sb, is sb.
func (sb *Sandbox) WithOrg(r OrgRoot) *Sandbox {
	if sb == nil || r.IsZero() {
		return sb
	}
	c := *sb
	c.Org = r
	c.TempDir = r.HomesDir()
	return &c
}

// ensureHomes makes a confined sandbox's HOMEs dir, 0700: a bind mount's
// parent, and nothing another org's session can list.
func (sb *Sandbox) ensureHomes() error {
	if sb.Org.IsZero() || sb.TempDir == "" {
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
// the session's org root: the first outside it refuses the whole list
// with a *MountOutsideOrgError naming the path and the org. An
// unconfined session's are Mounts' as they are.
func (s *Session) CheckedMounts(workspace string) ([]Mount, error) {
	named := s.namedMounts(workspace)
	out := make([]Mount, 0, len(named))
	for _, m := range named {
		if err := s.org.Check(m.what, m.Source); err != nil {
			return nil, err
		}
		out = append(out, m.Mount)
	}
	return out, nil
}
