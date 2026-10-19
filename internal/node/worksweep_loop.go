package node

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// DefaultWorkSweepEvery is how often the loop runs the work sweep after
// the start-up pass (mesh-cli ADR 008, The work sweep).
const DefaultWorkSweepEvery = time.Hour

// runWorkSweep runs l.WorkSweep at the end of a tick once WorkSweepEvery
// has passed on l.Now since the last pass. The first tick only starts the
// clock: `mesh node start` ran a pass at start-up, so the loop's first
// is an hour after it, and never two within the hour.
func (l *Loop) runWorkSweep(ctx context.Context) {
	if l.WorkSweep == nil {
		return
	}
	now := l.now()
	if l.workSweptAt.IsZero() {
		l.workSweptAt = now
		return
	}
	every := l.WorkSweepEvery
	if every <= 0 {
		every = DefaultWorkSweepEvery
	}
	if now.Sub(l.workSweptAt) < every {
		return
	}
	l.workSweptAt = now
	l.WorkSweep(ctx)
}

// LiveSubjects is the work sweep's live set: the item, goal and
// direction ids of every session this node runs — builds (RunningIDs),
// evaluations, plans, research, rebases and main validations — and of
// every session it adopted.
func (l *Loop) LiveSubjects() map[string]bool {
	ids := map[string]bool{}
	for _, id := range l.RunningIDs() {
		ids[id] = true
	}
	l.mu.Lock()
	for _, m := range []map[string]bool{l.evaluating, l.planning, l.researching, l.rebasing, l.validating} {
		for id := range m {
			ids[id] = true
		}
	}
	l.mu.Unlock()
	for _, a := range l.Adopted() {
		for _, id := range []string{a.ItemID, a.GoalID, a.DirectionID} {
			if id != "" {
				ids[id] = true
			}
		}
	}
	return ids
}

// AliveSessionSubjects is the live set of a work sweep run outside the
// node process (`mesh node sweep`): the item, goal and direction ids of
// every `.session` dir under root whose state's session is alive — its
// pid, or a sandboxed one's container through sb; with no sb to ask, a
// sandboxed session counts as alive — whatever node id it names.
func AliveSessionSubjects(root string, sb *sandbox.Sandbox) (map[string]bool, error) {
	ids := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if !d.IsDir() || path == root {
			return nil
		}
		if d.Name() == ScratchDirName && filepath.Dir(path) == root {
			return filepath.SkipDir
		}
		if strings.HasSuffix(d.Name(), ".session") {
			if st, err := sessionunit.ReadState(path); err == nil {
				f := FoundSession{Dir: path, State: st, Sandbox: sb}
				if (f.Sandboxed() && sb == nil) || f.Alive() {
					for _, id := range []string{st.ItemID, st.GoalID, st.DirectionID} {
						if id != "" {
							ids[id] = true
						}
					}
				}
			}
			return filepath.SkipDir
		}
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			return filepath.SkipDir // a checkout
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return ids, err
}
