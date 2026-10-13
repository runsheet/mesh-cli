package node

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// DefaultLiveSweepEvery is how many ticks apart the live probe's orphan
// sweep runs when LiveProbe.SweepEvery is unset.
const DefaultLiveSweepEvery = 5

// LiveProbe counts the build sessions actually alive on this host for
// this node (goal a-stalled-session-is-stopped-not-orphaned): the loop's
// own map counts only the builds it tracks, and on dl360 a capacity-8
// node ran nine `claude -p`. Every tick it lists what runs — the
// user manager's mesh-session-build-* scopes, or with no systemd the
// sessions under Root whose state file names this node and whose pid
// is alive — and the tick's free slots are capacity less the larger of
// the tracked builds and the live ones. A live build the loop neither
// runs nor adopted is an orphan: counted, and every SweepEvery ticks
// (the first included) looked up on the platform and stopped when its
// item is terminal, unknown, or held by another node.
type LiveProbe struct {
	// Systemctl is the user manager whose scopes are counted; nil is a
	// host with no systemd, where Find's sessions are counted instead.
	Systemctl sessionunit.Systemctl
	// Root is the work root the sessions' state files are under.
	Root string
	// Find lists the sessions under Root naming one of ids
	// (FindSessionsIn on Sandbox when nil).
	Find func(root string, ids []string) ([]FoundSession, error)
	// Sandbox is the node's sandbox: a build running detached in its pod
	// has no scope and no pid, and is counted by its container among
	// the found sessions, with systemd or without (nil: none).
	Sandbox *sandbox.Sandbox
	// SweepEvery is the ticks between orphan sweeps
	// (DefaultLiveSweepEvery when 0 or less).
	SweepEvery int

	ticks int
	// foreign are the scopes the last sweep found another node's — its
	// item held by a node not this one's and no state file of ours
	// naming it, as a second node on the same host runs — neither
	// counted nor stopped.
	foreign map[string]bool
	// said is the last capacity line logged, so it is said once while
	// its figures hold.
	said string
	// live and orphans are the last probe's figures (Loop.LiveSessions).
	live, orphans int
	// failSaid is set once a failed listing has been logged.
	failSaid bool
}

// liveSession is one build session alive on the host: its item, and
// either its scope's unit (without .scope) or its found session.
type liveSession struct {
	Item  string
	Unit  string
	Found *FoundSession
}

// name is the session's unit, else its session directory.
func (s liveSession) name() string {
	if s.Unit != "" {
		return s.Unit
	}
	if s.Found != nil {
		return s.Found.Dir
	}
	return s.Item
}

// LiveSessions is the last tick's live build sessions on the host and
// how many of them were orphans; both 0 without a probe.
func (l *Loop) LiveSessions() (live, orphans int) {
	if l.Live == nil {
		return 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Live.live, l.Live.orphans
}

// nodeIDs are every id this node registered under, its owned id first.
func (l *Loop) nodeIDs() []string {
	var ids []string
	if o := l.owned(); o != nil && o.NodeID != "" {
		ids = append(ids, o.NodeID)
	}
	for _, sc := range l.CurrentScopes() {
		if sc.NodeID != "" && !slices.Contains(ids, sc.NodeID) {
			ids = append(ids, sc.NodeID)
		}
	}
	return ids
}

// inUse is the build slots taken this tick: the tracked builds, or the
// live ones on the host when more run (orphans included). Without a
// probe, or when it cannot list, the tracked builds.
func (l *Loop) inUse(ctx context.Context) int {
	running := l.Running()
	p := l.Live
	if p == nil {
		return running
	}
	ids := l.nodeIDs()
	sessions, err := p.list(ids)
	if err != nil {
		if !p.failSaid {
			l.logf("capacity: live sessions unread (counting the tracked builds alone): %v", err)
			p.failSaid = true
		}
		return running
	}
	p.failSaid = false
	p.ticks++
	every := p.SweepEvery
	if every <= 0 {
		every = DefaultLiveSweepEvery
	}
	if (p.ticks-1)%every == 0 {
		sessions = l.sweepOrphans(ctx, p, sessions, ids)
	}
	live, orphans := 0, 0
	for _, s := range sessions {
		if p.foreign[s.name()] {
			continue
		}
		live++
		if !l.tracked(s.Item) {
			orphans++
		}
	}
	l.mu.Lock()
	p.live, p.orphans = live, orphans
	l.mu.Unlock()
	c := l.Capacity()
	if live >= c && live > 0 {
		line := fmt.Sprintf("capacity: %d live agent sessions (%d orphans) against capacity %d; claiming nothing", live, orphans, c)
		if line != p.said {
			l.logf("%s", line)
			p.said = line
		}
	} else {
		p.said = ""
	}
	return max(running, live)
}

// tracked is whether the loop runs or adopted a build of item.
func (l *Loop) tracked(item string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running[item] || l.adoptedIDs[item]
}

// list is the build sessions alive on the host: the active
// mesh-session-build-* scopes, or with no systemd the found build
// sessions whose process runs.
func (p *LiveProbe) list(ids []string) ([]liveSession, error) {
	if p.Systemctl != nil {
		scopes, err := p.Systemctl.ListScopes()
		if err != nil {
			return nil, err
		}
		var out []liveSession
		for _, sc := range scopes {
			if sc.Active == "inactive" || sc.Active == "failed" {
				continue
			}
			unit := strings.TrimSuffix(sc.Unit, ".scope")
			kind, subject, ok := sessionunit.ParseUnitName(unit)
			if !ok || harness.Kind(kind) != harness.KindBuild {
				continue
			}
			out = append(out, liveSession{Item: subject, Unit: unit})
		}
		if p.Sandbox == nil {
			return out, nil
		}
		// a sandboxed build runs in no scope: its container, once
		found, err := p.find(ids)
		if err != nil {
			return nil, err
		}
		for i := range found {
			s := found[i]
			if !s.Sandboxed() || harness.Kind(s.State.Kind) != harness.KindBuild || !s.Alive() {
				continue
			}
			if slices.ContainsFunc(out, func(o liveSession) bool { return o.Item == s.State.ItemID }) {
				continue
			}
			out = append(out, liveSession{Item: s.State.ItemID, Found: &s})
		}
		return out, nil
	}
	found, err := p.find(ids)
	if err != nil {
		return nil, err
	}
	var out []liveSession
	for i := range found {
		s := found[i]
		if harness.Kind(s.State.Kind) != harness.KindBuild || !s.Alive() {
			continue
		}
		out = append(out, liveSession{Item: s.State.ItemID, Found: &s})
	}
	return out, nil
}

func (p *LiveProbe) find(ids []string) ([]FoundSession, error) {
	if p.Find == nil {
		return FindSessionsIn(p.Root, ids, p.Sandbox)
	}
	found, err := p.Find(p.Root, ids)
	for i := range found {
		if found[i].Sandbox == nil {
			found[i].Sandbox = p.Sandbox
		}
	}
	return found, err
}

// sweepOrphans looks each untracked live session up on the platform and
// stops it — its whole unit, by stopScope or FoundSession.Kill, each
// logged — when its item is terminal or unknown, or held by another
// node while a state file of this node's names it (the platform gave
// the item away; the session is ours and stale). A scope whose item
// another node holds and no state file of ours names is another node's
// on the same host: left, and not counted (foreign). A live item of
// this node's is left and counted; an unread one is left. It returns
// the sessions still running.
func (l *Loop) sweepOrphans(ctx context.Context, p *LiveProbe, sessions []liveSession, ids []string) []liveSession {
	ours := map[string]bool{}
	if p.Systemctl != nil {
		if found, err := p.find(ids); err == nil {
			for _, f := range found {
				if f.State.Unit != "" {
					ours[strings.TrimSuffix(f.State.Unit, ".scope")] = true
				}
			}
		}
	}
	p.foreign = map[string]bool{}
	var left []liveSession
	for _, s := range sessions {
		if l.tracked(s.Item) {
			left = append(left, s)
			continue
		}
		j := judgeItem(ctx, l.GW, string(harness.KindBuild), s.Item)
		stop := false
		switch j.Verdict {
		case ScopeTerminal, ScopeUnknown:
			stop = true
		case ScopeLive:
			if j.NodeID != "" && !slices.Contains(ids, j.NodeID) {
				if s.Found != nil || ours[s.Unit] {
					stop = true
					j.Status += " on another node"
				} else {
					p.foreign[s.name()] = true
				}
			}
		case ScopeUnread:
			l.logf("capacity: orphan %s left, item %s unread: %v", s.name(), s.Item, j.Err)
		}
		if !stop || l.tracked(s.Item) {
			left = append(left, s)
			continue
		}
		var ok bool
		if s.Unit != "" {
			ok = stopScope(p.Systemctl, s.Unit, j, "capacity: orphan", l.logf)
		} else {
			killed, err := s.Found.Kill()
			ok = logStopped(s.name(), j, killed, err, "capacity: orphan", l.logf)
		}
		if !ok {
			left = append(left, s)
		}
	}
	return left
}
