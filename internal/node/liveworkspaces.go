package node

import (
	"path/filepath"
	"sort"
	"sync"
)

// liveWorkspaces is the node's set of item checkouts whose item is
// claimed, running, evaluating or rebasing here (SessionLive): each held
// by the session that works in it, for as long as it does. Process-wide,
// as the repo lock's mutex is, because the Runner, the Evaluator and the
// Rebaser each hold their own Git; a cleanup on any of them consults the
// one set and refuses to touch a dir in it or its .git/worktrees admin
// dir.
var liveWorkspaces = struct {
	sync.Mutex
	dirs map[string]int
}{dirs: map[string]int{}}

// HoldWorkspace marks dir live on this node until the returned release
// is called; holds count, so two sessions of one dir (a build and its
// adopted twin) release it only when both have.
func HoldWorkspace(dir string) (release func()) {
	key := liveKey(dir)
	liveWorkspaces.Lock()
	liveWorkspaces.dirs[key]++
	liveWorkspaces.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			liveWorkspaces.Lock()
			defer liveWorkspaces.Unlock()
			if liveWorkspaces.dirs[key]--; liveWorkspaces.dirs[key] <= 0 {
				delete(liveWorkspaces.dirs, key)
			}
		})
	}
}

// WorkspaceLive is whether dir is held by a session live on this node.
func WorkspaceLive(dir string) bool {
	key := liveKey(dir)
	liveWorkspaces.Lock()
	defer liveWorkspaces.Unlock()
	return liveWorkspaces.dirs[key] > 0
}

// LiveWorkspaces is every dir held live on this node, sorted.
func LiveWorkspaces() []string {
	liveWorkspaces.Lock()
	defer liveWorkspaces.Unlock()
	dirs := make([]string, 0, len(liveWorkspaces.dirs))
	for d := range liveWorkspaces.dirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// liveKey is dir absolute and cleaned, the symlinks of its deepest
// existing ancestor resolved, so a spec's Dir held before its parent
// exists, and a worktree's gitdir file read after, name it alike.
func liveKey(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	dir = filepath.Clean(dir)
	rest := ""
	for p := dir; ; p = filepath.Dir(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		if filepath.Dir(p) == p {
			return dir
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}
