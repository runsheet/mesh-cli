package node

// The start-up scratch sweep (goal
// a-host-sessions-scratch-dies-with-the-session): a node killed in the
// middle of a session never ran its end, so the session's scratch — its
// TMPDIR under the scratch root, its CLAUDE_CONFIG_DIR and GH_CONFIG_DIR
// and a no-workspace session's dir under /tmp — stays on disk. At start,
// once the sessions about to be adopted are found, every such dir that
// none of them names is removed, on every node, sandboxed or not.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// HostScratchPrefixes are the names the node itself makes its own dirs
// with directly under os.TempDir(): a no-workspace session's dir
// (mesh-session-*), a session's GH_CONFIG_DIR (mesh-gh-*) and
// CLAUDE_CONFIG_DIR (mesh-claude-*), and an image pull's, build's and
// push's work dirs (mesh-node-pull-*, mesh-node-image-*,
// mesh-node-push-*). mesh-home-* is added on a host node only: on a
// sandboxed one the sandbox's sweep knows which HOME a running
// container mounts.
var HostScratchPrefixes = []string{"mesh-session-", "mesh-gh-", "mesh-claude-", "mesh-node-pull-", "mesh-node-image-", "mesh-node-push-"}

// ScratchSweep is one start-up sweep of the host's scratch.
type ScratchSweep struct {
	// Roots are the scratch roots (ScratchRoot, OrgRoots.Scratch): every
	// dir directly under one is a run's scratch, swept unless kept.
	Roots []string
	// TempDir is where the node's own prefixed dirs are swept;
	// os.TempDir() when empty.
	TempDir string
	// Sandboxed leaves mesh-home-* under TempDir to SweepSandbox.
	Sandboxed bool
	// Found are the sessions the node is about to adopt: every path one
	// names — its dir, workspace, HOME, TempDirs, token file — is kept.
	Found []FoundSession
	// InUse says a path is held live by this process (a running
	// validation's scratch); kept. Nil is none.
	InUse func(path string) bool
	// Owned says an entry is the node's user's; ownedByNodeUID when
	// nil. Another user's dir is never touched.
	Owned func(os.FileInfo) bool
}

// ScratchSweepResult is what a scratch sweep removed and kept.
type ScratchSweepResult struct {
	Dirs  int
	Bytes int64
	Kept  int
}

// Line is the node's log line for the sweep.
func (r ScratchSweepResult) Line() string {
	return fmt.Sprintf("scratch: swept %d dirs, %d bytes at start (%d kept)", r.Dirs, r.Bytes, r.Kept)
}

// keptPaths are every path a found session names, and the process's own
// HOME: a session or test whose HOME is a mesh-home-* dir under /tmp (a
// pod's HOME mounted at its host path) never has it swept from under it.
func (s ScratchSweep) keptPaths() []string {
	var kept []string
	add := func(p string) {
		if p != "" {
			kept = append(kept, filepath.Clean(p))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
	}
	for _, f := range s.Found {
		add(f.Dir)
		add(f.State.Workspace)
		add(f.State.Home)
		for _, d := range f.State.TempDirs {
			add(d)
		}
		if f.State.TokenFile != "" {
			add(filepath.Dir(f.State.TokenFile))
		}
	}
	return kept
}

// Run sweeps: every dir directly under each root, and every dir under
// TempDir named with one of the node's prefixes, owned by the node's
// user, that no kept path is or lies inside and InUse does not hold, is
// removed with everything in it. A symlink is never followed or removed;
// a dir of another prefix (runsheet-*, a project's own) is left to the
// quartermaster's age-bounded scratch check. A removal that fails is in
// the error and the sweep goes on.
func (s ScratchSweep) Run() (ScratchSweepResult, error) {
	var res ScratchSweepResult
	kept := s.keptPaths()
	owned := s.Owned
	if owned == nil {
		owned = ownedByNodeUID
	}
	prefixes := HostScratchPrefixes
	if !s.Sandboxed {
		prefixes = append(append([]string(nil), prefixes...), sandbox.HomePrefix)
	}
	tempDir := s.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	var errs []string
	sweep := func(dir string, match func(name string) bool) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, err.Error())
			}
			return
		}
		for _, e := range entries {
			if !e.IsDir() || !match(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil || !owned(info) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if holdsKept(path, kept) || (s.InUse != nil && s.InUse(path)) {
				res.Kept++
				continue
			}
			size := dirSize(path)
			if err := RemoveScratch(path); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			res.Dirs++
			res.Bytes += size
		}
	}
	for _, root := range s.Roots {
		if root != "" {
			sweep(root, func(string) bool { return true })
		}
	}
	sweep(tempDir, func(name string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	})
	if len(errs) > 0 {
		return res, fmt.Errorf("scratch sweep: %s", strings.Join(errs, "; "))
	}
	return res, nil
}

// holdsKept is path a kept path, or a kept path inside it.
func holdsKept(path string, kept []string) bool {
	for _, k := range kept {
		if k == path || strings.HasPrefix(k, path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// SweepScratch runs the start-up scratch sweep and logs its line, and
// any failure on one line before it.
func SweepScratch(s ScratchSweep, logf func(string, ...any)) ScratchSweepResult {
	res, err := s.Run()
	if err != nil {
		logf("scratch: sweep at start: %s", strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	logf("%s", res.Line())
	return res
}
