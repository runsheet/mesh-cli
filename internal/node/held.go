package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// HeldGoal is a goal this node holds off claiming for — a plan claim
// released by a refusal that keeps repeating (ReleaseGoalPlan), or a
// work item claim refused for budget, pause or approval — until the
// goal's row changes or the item is listed claimable again. Kind is
// "plan" or "item". `mesh node status` lists them with Reason; node.log
// says so as each is noted and cleared (goal
// a-refused-plan-is-released-not-replanned).
type HeldGoal struct {
	GoalID string
	Slug   string
	Org    string
	Kind   string
	Reason string
	Since  time.Time
	Until  time.Time
}

// HeldPath is where Tick leaves the held goals for `mesh node status`:
// node.held.
func (s RunState) HeldPath() string { return filepath.Join(s.Dir, "node.held") }

// WriteHeld leaves the node's held goals for `mesh node status`, atomic
// like WriteScaling; an empty list removes the file.
func (s RunState) WriteHeld(held []HeldGoal) error {
	if len(held) == 0 {
		if err := os.Remove(s.HeldPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(held)
	if err != nil {
		return err
	}
	tmp := s.HeldPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.HeldPath())
}

// Held reads what WriteHeld left; false when nothing is there or it
// cannot be read.
func (s RunState) Held() ([]HeldGoal, bool) {
	data, err := os.ReadFile(s.HeldPath())
	if err != nil {
		return nil, false
	}
	var held []HeldGoal
	if json.Unmarshal(data, &held) != nil {
		return nil, false
	}
	return held, true
}

// NoteHeld records a goal and kind as held, replacing any entry of the
// same GoalID and Kind. The first time a goal and kind is held (no
// entry replaced) it is logged; a later call that only updates the
// reason or until is not.
func (l *Loop) NoteHeld(h HeldGoal) {
	l.mu.Lock()
	found := false
	for i := range l.held {
		if l.held[i].GoalID == h.GoalID && l.held[i].Kind == h.Kind {
			l.held[i] = h
			found = true
			break
		}
	}
	if !found {
		l.held = append(l.held, h)
	}
	l.mu.Unlock()
	if !found {
		l.logf("held: %s goal %s (%s): %s", h.Org, h.Slug, h.Kind, h.Reason)
	}
}

// ClearHeld releases the hold on a goal and kind, logging its release
// when there was one.
func (l *Loop) ClearHeld(goalID, kind string) {
	l.mu.Lock()
	var cleared *HeldGoal
	kept := make([]HeldGoal, 0, len(l.held))
	for _, h := range l.held {
		if h.GoalID == goalID && h.Kind == kind {
			c := h
			cleared = &c
			continue
		}
		kept = append(kept, h)
	}
	l.held = kept
	l.mu.Unlock()
	if cleared != nil {
		l.logf("held: %s goal %s (%s) released", cleared.Org, cleared.Slug, cleared.Kind)
	}
}

// HeldGoals is the node's held goals, sorted by org, slug, kind.
func (l *Loop) HeldGoals() []HeldGoal {
	l.mu.Lock()
	out := make([]HeldGoal, len(l.held))
	copy(out, l.held)
	l.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Org != out[j].Org {
			return out[i].Org < out[j].Org
		}
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}
