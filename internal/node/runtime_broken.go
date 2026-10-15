package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/validate"
)

// RuntimeProbeEvery is how often the loop asks the container runtime
// whether it can start a session's pod (Loop.RuntimeProbe, item 1's
// Sandbox.Probe): a node started on a broken podman is latched before
// it claims, and a broken one is seen healed within this — so the state
// is on the platform within ten minutes either way (ADR-034).
const RuntimeProbeEvery = 5 * time.Minute

// RuntimeProbeTimeout bounds one probe: a podman that hangs on its
// store is as broken as one that refuses.
const RuntimeProbeTimeout = time.Minute

// RuntimeBrokenStatus is the heartbeat's `status` while the latch is
// set, sent only when the gateway takes it (TakesNodeStatus).
const RuntimeBrokenStatus = string(validate.RuntimeBroken)

// RuntimeState is the node's container runtime when it cannot start a
// session: the status (runtime_broken), podman's words and since when.
// It is what node.runtime holds for `mesh node status` (ADR-034).
type RuntimeState struct {
	Status string    `json:"status"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// Summary is the line the holding report opens with and every
// unclaimed row carries: `runtime_broken: <podman's words>`.
func (s RuntimeState) Summary() string { return RuntimeBrokenPrefix + s.Reason }

// statusTaker is the gateway's answer to which heartbeat statuses it
// folds (gateway.Client.TakesNodeStatus); a Gateway without it takes
// none but harness_unavailable.
type statusTaker interface {
	TakesNodeStatus(ctx context.Context, status string) bool
}

// runtimeWords is err as the latch keeps it: podman's own words,
// without the runtime_broken prefix the release reason opens with.
func runtimeWords(err error) string {
	var why string
	if re := RuntimeErrorOf(err); re != nil {
		why = RuntimeReason(re)
	} else {
		why = truncate(scrubbed(err, nil).Error(), 2000)
	}
	return oneLine(strings.TrimPrefix(why, RuntimeBrokenPrefix))
}

// NoteRuntimeBroken latches the node's runtime as broken — told by a
// session whose pod could not start (the Runner's, Evaluator's,
// Planner's and Rebaser's RuntimeBroken hooks) or by a failed probe.
// While it is set Tick claims nothing of any kind (items, evaluations,
// plans, rebases, research, main validations), as a drain does for new
// starts, and the heartbeat says runtime_broken; only a probe that
// passes clears it. The first note logs once; a later one updates the
// reason and keeps the since.
func (l *Loop) NoteRuntimeBroken(err error) {
	if err == nil {
		return
	}
	words := runtimeWords(err)
	l.mu.Lock()
	first := l.runtime == nil
	if first {
		l.runtime = &RuntimeState{Status: RuntimeBrokenStatus, Reason: words, Since: l.now().UTC()}
	} else {
		l.runtime.Reason = words
	}
	l.mu.Unlock()
	if first {
		l.logf("runtime broken: %s; claiming nothing until a probe passes (every %s)", words, l.runtimeProbeEvery())
	}
}

// RuntimeBroken is the latched runtime state, nil while the runtime is
// healthy.
func (l *Loop) RuntimeBroken() *RuntimeState {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.runtime == nil {
		return nil
	}
	s := *l.runtime
	return &s
}

func (l *Loop) runtimeProbeEvery() time.Duration {
	if l.RuntimeProbeEvery > 0 {
		return l.RuntimeProbeEvery
	}
	return RuntimeProbeEvery
}

// probeRuntime asks RuntimeProbe, every runtimeProbeEvery by the
// loop's clock (the first tick at once): a failure latches the runtime
// broken even before any session tried, a pass clears a latch — logged
// `runtime healed`, the runtime_broken holds on items and goals lifted
// so they are claimed again — and the state is told to RuntimeChanged
// either way. A nil RuntimeProbe (no sandbox) probes nothing.
func (l *Loop) probeRuntime(ctx context.Context) {
	if l.RuntimeProbe != nil {
		now := l.now()
		l.mu.Lock()
		due := l.runtimeProbedAt.IsZero() || now.Sub(l.runtimeProbedAt) >= l.runtimeProbeEvery()
		if due {
			l.runtimeProbedAt = now
		}
		l.mu.Unlock()
		if due {
			pctx, cancel := context.WithTimeout(ctx, RuntimeProbeTimeout)
			err := l.RuntimeProbe(pctx)
			cancel()
			switch {
			case err != nil && ctx.Err() == nil:
				l.NoteRuntimeBroken(err)
			case err == nil:
				l.healRuntime()
			}
		}
	}
	if l.RuntimeChanged != nil {
		l.RuntimeChanged(l.RuntimeBroken())
	}
}

// healRuntime clears the latch after a probe passed.
func (l *Loop) healRuntime() {
	l.mu.Lock()
	was := l.runtime
	l.runtime = nil
	l.mu.Unlock()
	if was == nil {
		return
	}
	n := l.Declines.ForgetPrefix(RuntimeBrokenPrefix)
	l.logf("runtime healed: broken since %s (%s); claiming again, %d runtime_broken hold(s) lifted", was.Since.Format(time.RFC3339), was.Reason, n)
}

// runtimeHeartbeat puts the latch on the heartbeat: Status
// runtime_broken with podman's words in Reason when the gateway takes
// the status, else the words, prefixed, in Reason alone — a condition
// while the node stays online, as the sandbox's limit kill is — never a
// status the platform would refuse (ADR-034).
func (l *Loop) runtimeHeartbeat(ctx context.Context, x *gateway.HeartbeatExtras, rt *RuntimeState) {
	if rt == nil {
		return
	}
	if st, ok := l.GW.(statusTaker); ok && st.TakesNodeStatus(ctx, RuntimeBrokenStatus) {
		x.Status, x.Reason = RuntimeBrokenStatus, rt.Reason
		return
	}
	x.Reason = rt.Summary()
}

// passOverRuntime records, on the holding report, the ready items every
// served org has while the runtime is broken: one row per goal, its
// reason the latch's `runtime_broken: …`. Nothing is claimed.
func (l *Loop) passOverRuntime(ctx context.Context, rt *RuntimeState) {
	if !l.takesKind("build") {
		return
	}
	why := rt.Summary()
	for _, sc := range l.Scopes {
		items, err := l.GW.ReadyWorkItems(ctx, sc.ID)
		if err != nil {
			l.logf("list ready items of %s: %v", sc.Name(), err)
			continue
		}
		for _, it := range items {
			l.passOverReason(it.GoalID, "item", why)
		}
	}
}

// passOverReason folds one passed-over subject into its goal and kind's
// row with reason as given.
func (l *Loop) passOverReason(goalID, kind, why string) {
	for i := range l.unclaimed {
		if u := &l.unclaimed[i]; u.GoalID == goalID && u.Kind == kind {
			u.Count++
			return
		}
	}
	l.unclaimed = append(l.unclaimed, Unclaimed{GoalID: goalID, Kind: kind, Count: 1, Reason: why})
}

// runtimeHolding opens the holding report's summary with the latch:
// `runtime_broken: <podman's words>; nothing claimed until a probe
// passes; …` the rest of the judgement after it.
func runtimeHolding(h gateway.Holding, rt *RuntimeState) gateway.Holding {
	h.Summary = rt.Summary() + "; nothing claimed until a probe passes; " + h.Summary
	for i := range h.Unclaimed {
		h.Unclaimed[i].Reason = rt.Summary()
	}
	return h
}

// ForgetPrefix drops every hold whose reason opens with prefix — the
// runtime_broken holds once the runtime heals — and answers how many.
func (d *Declines) ForgetPrefix(prefix string) int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for id, dc := range d.m {
		if strings.HasPrefix(dc.reason, prefix) {
			delete(d.m, id)
			n++
		}
	}
	return n
}

// RuntimePath is the node's broken runtime as of the last tick:
// node.runtime, beside node.standing (ADR-034).
func (s RunState) RuntimePath() string { return filepath.Join(s.Dir, "node.runtime") }

// WriteRuntime leaves the latched runtime state for `mesh node status`,
// atomic like WriteStanding; nil — healthy, or healed — removes it.
func (s RunState) WriteRuntime(rt *RuntimeState) error {
	if rt == nil {
		if err := os.Remove(s.RuntimePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(rt)
	if err != nil {
		return err
	}
	tmp := s.RuntimePath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.RuntimePath())
}

// Runtime reads what WriteRuntime left; false when nothing is there or
// it cannot be read.
func (s RunState) Runtime() (RuntimeState, bool) {
	data, err := os.ReadFile(s.RuntimePath())
	if err != nil {
		return RuntimeState{}, false
	}
	var rt RuntimeState
	if json.Unmarshal(data, &rt) != nil || rt.Status == "" {
		return RuntimeState{}, false
	}
	return rt, true
}
