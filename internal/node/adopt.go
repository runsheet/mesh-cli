package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"projectmesh.io/mesh-cli/internal/crash"
	"slices"
	"sort"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// errNoAttach is a harness that cannot take up a session it did not
// start (only a host Claude session can be adopted today).
var errNoAttach = errors.New("adopt: the harness cannot attach to a running session")

// FoundSession is a session directory under the work root whose state
// file names this node (mesh-cli ADR 009): Dir is `<workdir>.session`.
type FoundSession struct {
	Dir   string
	State sessionunit.State
	// Unit is the unit the session runs in; nil is the one its state's
	// runner names.
	Unit sessionunit.Unit
}

// unit is the unit the session runs in, by its state's runner.
func (s FoundSession) unit() sessionunit.Unit {
	if s.Unit != nil {
		return s.Unit
	}
	if s.State.Runner == (sessionunit.SystemdScope{}).Name() {
		return sessionunit.SystemdScope{}
	}
	return sessionunit.Detached{}
}

func (s FoundSession) handle() sessionunit.Handle {
	return sessionunit.Handle{PID: s.State.PID, UnitName: s.State.Unit}
}

// Alive is whether the session's process still runs.
func (s FoundSession) Alive() bool { return s.State.PID > 0 && s.unit().Alive(s.handle()) }

// Liveness is "running" or "ended", for the log.
func (s FoundSession) Liveness() string {
	if s.Alive() {
		return "running"
	}
	return "ended"
}

// Kill ends the session's whole unit — its claude, when it still runs,
// and whatever the agent left running there, which may outlive claude —
// and returns what it killed besides claude (ADR 008, The session's
// end). A unit already gone is nothing and no error.
func (s FoundSession) Kill() ([]sessionunit.Process, error) {
	if s.State.PID <= 0 && s.State.Unit == "" {
		return nil, nil
	}
	return s.unit().Kill(s.handle())
}

// Subject is what the session is for: its item, a plan's goal or a
// research session's direction.
func (s FoundSession) Subject() string {
	switch harness.Kind(s.State.Kind) {
	case harness.KindPlan:
		return s.State.GoalID
	case harness.KindResearch:
		return s.State.DirectionID
	}
	return s.State.ItemID
}

// FindSessions lists every session directory under root whose state file
// names one of nodeIDs, oldest first. A git checkout is not searched
// inside (a session directory is its sibling), nor is a session
// directory; an unreadable state file is skipped.
func FindSessions(root string, nodeIDs []string) ([]FoundSession, error) {
	var found []FoundSession
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
		if strings.HasSuffix(d.Name(), ".session") {
			if st, err := sessionunit.ReadState(path); err == nil && slices.Contains(nodeIDs, st.NodeID) && st.NodeID != "" {
				found = append(found, FoundSession{Dir: path, State: st})
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
	sort.SliceStable(found, func(i, j int) bool { return found[i].State.StartedAt.Before(found[j].State.StartedAt) })
	return found, err
}

// stateAccount is the account a session directory's state names
// (DefaultAccount when none).
func stateAccount(dir string) string {
	if st, err := sessionunit.ReadState(dir); err == nil && st.Account != "" {
		return st.Account
	}
	return DefaultAccount
}

// adoptedEntry is one adopted session kept in Loop.adopted: the
// heartbeat fields that do not change (item/goal/direction id, kind,
// since) and the FoundSession, re-read on every heartbeat for its
// SessionID and liveness (Loop.Adopted, mesh-cli ADR-009 amendment).
type adoptedEntry struct {
	session gateway.AdoptedSession
	found   FoundSession
}

// AdoptSessions takes up the sessions a node process before this one
// started (mesh-cli ADR 009), before the first tick claims anything:
// each is marked as the loop's own — a build counts in Running, every
// kind in the drain's sessions, and none is claimed or started again —
// every item's heartbeat is sent at once, so the platform's stall window
// does not run out across the restart, and AdoptSession then follows
// each to its end and runs what its kind runs after a session. The
// heartbeat lists them as `adopted` until they end. Without an
// AdoptSession nothing is adopted.
func (l *Loop) AdoptSessions(ctx context.Context, found []FoundSession) int {
	if l.AdoptSession == nil {
		return 0
	}
	n := 0
	for _, s := range found {
		subject := s.Subject()
		if subject == "" {
			continue
		}
		kind := harness.Kind(s.State.Kind)
		l.mu.Lock()
		held := l.adoptLocked(kind, subject, s.State.Account)
		if held == nil {
			l.mu.Unlock()
			continue
		}
		if l.adopted == nil {
			l.adopted = map[string]adoptedEntry{}
		}
		session := gateway.AdoptedSession{ItemID: subject, Kind: string(kind), Since: s.State.StartedAt.UTC(), SessionID: s.State.SessionID, Alive: s.Alive()}
		switch kind {
		case harness.KindPlan:
			session.GoalID = subject
		case harness.KindResearch:
			session.DirectionID = subject
		}
		l.adopted[subject] = adoptedEntry{session: session, found: s}
		l.mu.Unlock()
		if s.State.ItemID != "" && kind != harness.KindPlan && kind != harness.KindResearch {
			note := "session adopted by the restarted node (" + s.Liveness() + ")"
			hctx := ctx
			if kind == harness.KindBuild || kind == harness.KindRebase {
				hctx = gateway.WithActor(ctx, gateway.RoleBuilder, agentSlug(l.Cfg))
			}
			if err := l.GW.ReportWorkItemProgress(hctx, s.State.ItemID, note, 0, s.State.SessionID); err != nil {
				l.logf("item %s: adopted heartbeat: %v", s.State.ItemID, err)
			}
		}
		sessionNote := ""
		if s.State.SessionID != "" {
			sessionNote = fmt.Sprintf(", session %s", s.State.SessionID)
		}
		l.logf("adopted a %s session of %s (pid %d, %s%s)", kind, subject, s.State.PID, s.Liveness(), sessionNote)
		n++
		l.wg.Add(1)
		go func(s FoundSession) {
			defer crash.Guard()
			defer l.wg.Done()
			defer func() {
				// the claim's record, if the process before kept one, goes
				// with the session's handled end (ADR 009, Claims)
				if kind == harness.KindPlan || kind == harness.KindResearch {
					unlessDetached(ctx, func() { l.dropClaim(kind, subject) })
				}
				l.mu.Lock()
				delete(held, subject)
				delete(l.adopted, subject)
				if kind == harness.KindBuild {
					delete(l.runningOn, subject)
				}
				l.mu.Unlock()
				l.notify()
			}()
			l.AdoptSession(ctx, s)
		}(s)
	}
	return n
}

// adoptLocked marks subject as in flight in the map of its kind and
// returns that map, or nil when it is already there (or of no kind the
// loop runs). l.mu is held.
func (l *Loop) adoptLocked(kind harness.Kind, subject, account string) map[string]bool {
	var m *map[string]bool
	switch kind {
	case harness.KindBuild:
		m = &l.running
	case harness.KindEvaluate:
		m = &l.evaluating
	case harness.KindPlan:
		m = &l.planning
	case harness.KindResearch:
		m = &l.researching
	case harness.KindRebase:
		m = &l.rebasing
	default:
		return nil
	}
	if *m == nil {
		*m = map[string]bool{}
	}
	if (*m)[subject] {
		return nil
	}
	(*m)[subject] = true
	if l.adoptedIDs == nil {
		l.adoptedIDs = map[string]bool{}
	}
	l.adoptedIDs[subject] = true
	if kind == harness.KindBuild {
		if l.runningOn == nil {
			l.runningOn = map[string]string{}
		}
		if account == "" {
			account = DefaultAccount
		}
		l.runningOn[subject] = account
	}
	return *m
}

// Adopted is the heartbeat's `adopted`: the sessions taken over from the
// process before this one that have not yet ended, oldest first. Each
// entry's SessionID and Alive are re-read from its session's state.json
// and unit so a session id the agent names after AdoptSessions ran, and
// the session ending, both reach the next heartbeat (mesh-cli ADR-009
// amendment).
func (l *Loop) Adopted() []gateway.AdoptedSession {
	l.mu.Lock()
	entries := make([]adoptedEntry, 0, len(l.adopted))
	for _, e := range l.adopted {
		entries = append(entries, e)
	}
	l.mu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	out := make([]gateway.AdoptedSession, 0, len(entries))
	for _, e := range entries {
		a := e.session
		found := e.found
		if st, err := sessionunit.ReadState(found.Dir); err == nil {
			found.State = st
			if st.SessionID != "" {
				a.SessionID = st.SessionID
			}
		}
		a.Alive = found.Alive()
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].ItemID < out[j].ItemID
	})
	return out
}

// adoptItem reads the item an adopted session is for.
func adoptItem(ctx context.Context, gw Gateway, s FoundSession) (gateway.WorkItem, error) {
	it, err := gw.WorkItem(ctx, s.State.ItemID)
	if err != nil {
		return gateway.WorkItem{}, err
	}
	if it == nil {
		return gateway.WorkItem{}, errors.New("no such item")
	}
	return *it, nil
}

// AdoptWith is the loop's AdoptSession for the node's sessions: each
// kind to its own adopter (nil ones are stopped and removed). A session
// whose subject cannot be read is left in place for the next start.
type AdoptWith struct {
	GW       Gateway
	Scopes   func() Scopes
	Build    func(ctx context.Context, item gateway.WorkItem, s FoundSession) error
	Evaluate func(ctx context.Context, item gateway.WorkItem, s FoundSession) error
	Rebase   func(ctx context.Context, item gateway.WorkItem, s FoundSession) error
	Plan     func(ctx context.Context, goal gateway.Goal, scope Scope, s FoundSession) error
	Research func(ctx context.Context, d gateway.Direction, p gateway.Project, scope Scope, s FoundSession) error
	Logf     func(string, ...any)
	// Legacy maps a per-org node id the node registered under before it
	// was owned (Config.NodeIDIn) to its org's id: a session started
	// under it is its org's scope's still (ADR-015, Registration and the
	// loop). Nil on the legacy path.
	Legacy map[string]string
}

func (a AdoptWith) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// Adopt implements Loop.AdoptSession.
func (a AdoptWith) Adopt(ctx context.Context, s FoundSession) {
	err := a.adopt(ctx, s)
	if err != nil && ctx.Err() == nil {
		a.logf("adopted %s session of %s: %v", s.State.Kind, s.Subject(), err)
	}
}

func (a AdoptWith) adopt(ctx context.Context, s FoundSession) error {
	scope := func() (Scope, bool) {
		for _, sc := range a.Scopes() {
			if sc.NodeID == s.State.NodeID || (a.Legacy[s.State.NodeID] != "" && a.Legacy[s.State.NodeID] == sc.ID) {
				return sc, true
			}
		}
		return Scope{}, false
	}
	drop := func() error {
		stopFound(a.logf, s.State.Kind+" session of "+s.Subject(), s)
		return harness.RemoveSession(s.State.Workspace)
	}
	switch harness.Kind(s.State.Kind) {
	case harness.KindBuild, harness.KindEvaluate, harness.KindRebase:
		item, err := adoptItem(ctx, a.GW, s)
		if err != nil {
			return err
		}
		switch harness.Kind(s.State.Kind) {
		case harness.KindBuild:
			if a.Build != nil {
				return a.Build(ctx, item, s)
			}
		case harness.KindEvaluate:
			if a.Evaluate != nil {
				return a.Evaluate(ctx, item, s)
			}
		default:
			if a.Rebase != nil {
				return a.Rebase(ctx, item, s)
			}
		}
	case harness.KindPlan:
		sc, ok := scope()
		if !ok || a.Plan == nil {
			break
		}
		goal, err := a.GW.Goal(ctx, s.State.GoalID)
		if err != nil {
			return err
		}
		return a.Plan(ctx, *goal, sc, s)
	case harness.KindResearch:
		sc, ok := scope()
		if !ok || a.Research == nil {
			break
		}
		directions, err := a.GW.Directions(ctx, sc.ID, "")
		if err != nil {
			return err
		}
		for _, d := range directions {
			if d.ID != s.State.DirectionID {
				continue
			}
			p, err := a.GW.Project(ctx, d.ProjectID)
			if err != nil {
				return err
			}
			return a.Research(ctx, d, *p, sc, s)
		}
	}
	a.logf("adopted %s session of %s: nothing here takes it up; stopped", s.State.Kind, s.Subject())
	return drop()
}

// unlessDetached runs fn unless ctx is done: a session detached at the
// node's shutdown still runs in the dirs fn would remove, and the node
// that ends it removes them (harness.RemoveSession, the state's
// tempDirs).
func unlessDetached(ctx context.Context, fn func()) {
	if ctx.Err() == nil {
		fn()
	}
}

// sessionTempDirs are the temporary dirs a session runs in — its
// CLAUDE_CONFIG_DIR and GH_CONFIG_DIR — for its state file, so the node
// that ends a detached session removes them.
func sessionTempDirs(job harness.Job, acct *SessionAccount) []string {
	var dirs []string
	if acct != nil && acct.AccountSession != nil && acct.ConfigDir != "" {
		dirs = append(dirs, acct.ConfigDir)
	}
	for _, kv := range job.Env {
		if v, ok := strings.CutPrefix(kv, "GH_CONFIG_DIR="); ok && v != "" {
			dirs = append(dirs, v)
		}
	}
	return dirs
}
