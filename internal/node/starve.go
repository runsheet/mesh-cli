package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// ErrStarved is the cause a build's session ctx is cancelled with when
// it has written nothing to its stream for node.yaml's
// host.starve_after while the host is overloaded: the host starved it,
// not the item. It Is harness.ErrStopSession, so the unit is stopped and
// killed rather than detached, and the item is released — the attempt
// not counted (trd §3) — with the overload as its reason (ADR-029).
var ErrStarved = fmt.Errorf("starved on an overloaded host: %w", harness.ErrStopSession)

// StallPrefix opens the release note of a starved build: the platform's
// own word for a session that went quiet, followed by the overload
// reason (MachineReading.Overloaded), `stalled: host overloaded: …`.
const StallPrefix = "stalled: "

// hostConfig is node.yaml's `host:` in force for the runner: Host when
// set (Loop.HostThresholds, reloaded each tick), else Cfg's, defaults
// filled.
func (r *Runner) hostConfig() HostConfig {
	if r.Host != nil {
		return r.Host().Effective()
	}
	if r.Cfg != nil {
		return r.Cfg.Host.Effective()
	}
	return HostConfig{}.Effective()
}

// overloaded reads the machine now and says whether it is past
// node.yaml's overload threshold, and why. No Machine, or a failed
// read, is never overloaded.
func (r *Runner) overloaded() (bool, string) {
	if r.Machine == nil {
		return false, ""
	}
	m, err := r.Machine.Read()
	if err != nil {
		return false, ""
	}
	return m.Overloaded(r.hostConfig())
}

// heard marks the session's stream as having said something now.
func (b *buildRun) heard() {
	b.lastHeard.Store(time.Now().UnixNano())
}

// silentFor is how long the session has written nothing to its stream:
// since the later of the last event the harness passed on and the last
// write to its output.jsonl, which also catches the events the harness
// does not pass on (a tool call, a tool result).
func (b *buildRun) silentFor(now time.Time) time.Duration {
	last := time.Unix(0, b.lastHeard.Load())
	if b.dir != "" {
		if fi, err := os.Stat(filepath.Join(sessionunit.Dir(b.dir), sessionunit.OutputFile)); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	return now.Sub(last)
}

// checkStarved is one tick of a build's starve check: a session silent
// for host.starve_after on a host overloaded at that moment is given up
// with ErrStarved and the overload reason; a silent session on a host
// that is not overloaded is left to run. It reports whether the session
// still runs.
func (b *buildRun) checkStarved() bool {
	after := b.r.hostConfig().StarveAfter
	if after <= 0 || b.silentFor(time.Now()) < after {
		return true
	}
	over, why := b.r.overloaded()
	if !over {
		return true
	}
	b.watch.starve(why)
	return false
}

// afterStarved ends a build the host starved: what its unit's kill found
// is logged, its session directory goes, and the item is released with
// `stalled: <overload reason>` — a release, so no attempt is charged,
// and no fail or finish is sent.
func (r *Runner) afterStarved(ctx context.Context, b *buildRun, why string, result harness.Result) error {
	reason := StallPrefix + why
	r.logf("item %s: stalled on an overloaded host; released: %s", b.label, reason)
	logKilled(r.logf, "item "+b.label, result.KilledPerSession(), result.KillError)
	_ = harness.RemoveSession(b.dir)
	_ = handInRefused(r.logf, nil, b.label, "ReleaseWorkItem", r.GW.ReleaseWorkItem(ctx, b.item.ID, b.nodeID, reason))
	return nil
}
