package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// handoffFile is the name, under the lease's own directory
// (Runtime.leaseDir), of what Upgrade leaves for a lease it does not
// hand back.

const handoffFile = "handoff.json"

// handoffState is what an upgrade leaves of a lease it chose to leave
// running rather than hand back: its brief, where its sessionproxy
// listens, where its job found its push_to, and its turn ledger -- what
// a later node would need to pick the lease back up without asking Mesh
// for any of it again. Picking it back up is staged separately
// (ADR-044); this only writes what it would need.
type handoffState struct {
	Brief     *meshapi.Brief    `json:"brief"`
	SocketDir string            `json:"socketDir"`
	PushHeads map[string]string `json:"pushHeads,omitempty"`
	Turns     *handoffTurns     `json:"turns,omitempty"`
	// SidecarDir is the lease's relay sidecar's socket directory
	// (relaypod.go's relaySidecar.dir), "" for a lease with no sidecar
	// (plain sessionproxy mode): such a lease cannot be adopted
	// (adopt.go) and is handed back node_update at start instead.
	SidecarDir string `json:"sidecarDir,omitempty"`
	// EgressPID and EgressControl name the lease's egress process
	// (ADR-051 decisions 1 and 3), beside SidecarDir: "" for a lease
	// whose relay ran in-process, or with no sidecar at all. adoptOne
	// (decision 4) pushes it fresh credentials over EgressControl and
	// forwards the adopted node socket's CONNECTs onto its own node
	// socket, as long as the process named by EgressPID is still
	// alive; one that has died is adopted anyway, with no egress
	// tunnel from then on.
	EgressPID     int    `json:"egressPid,omitempty"`
	EgressControl string `json:"egressControl,omitempty"`
	// AgentStdoutOffset is how far into the lease's agent output
	// (agentOutputPath, the agent's own stdout file) the node had got,
	// so an adopting node follows it from there rather than from the top
	// (adopt.go's followAgentOutput).
	AgentStdoutOffset int64 `json:"agentStdoutOffset,omitempty"`
	// Lanes is the validation's lanes running beside the lease's pod
	// when the hand-off was written (admit.go): an adopting node keeps
	// their pods and holds their room again (adoptLanes).
	Lanes *handoffLanes `json:"lanes,omitempty"`
}

// handoffTurns is the lease's turnLedger as written to disk: seen, and
// the seqs above it the session was already given (given) or refused
// for a closed input (closed). pending is not meaningful once the
// process that held it is gone, so it is not written.
type handoffTurns struct {
	Seen   int64   `json:"seen"`
	Given  []int64 `json:"given,omitempty"`
	Closed []int64 `json:"closed,omitempty"`
}

// turnsForHandoff reads l's ledger into its disk shape, nil for a lease
// with none.
func turnsForHandoff(l *turnLedger) *handoffTurns {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h := &handoffTurns{Seen: l.seen}
	for seq := range l.given {
		h.Given = append(h.Given, seq)
	}
	for seq := range l.closed {
		h.Closed = append(h.Closed, seq)
	}
	sort.Slice(h.Given, func(i, j int) bool { return h.Given[i] < h.Given[j] })
	sort.Slice(h.Closed, func(i, j int) bool { return h.Closed[i] < h.Closed[j] })
	return h
}

// writeHandoff leaves l's hand-off state under its own lease directory,
// atomically: a reader never sees a half-written file.
func (r *Runtime) writeHandoff(id string, l *running) error {
	st := &handoffState{Brief: l.brief, SocketDir: filepath.Dir(l.sock), PushHeads: l.pushHeads, SidecarDir: l.sidecarDir, EgressPID: l.egressPID, EgressControl: l.egressControl, Lanes: r.lanes.of(id)}
	if l.mailbox != nil {
		st.Turns = turnsForHandoff(l.mailbox.turns)
	}
	if fi, err := os.Stat(r.agentOutputPath(id)); err == nil {
		st.AgentStdoutOffset = fi.Size()
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := r.leaseDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, handoffFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, handoffFile))
}

// agentOutputPath is the lease's agent's own stdout, under the lease's
// directory (agentio.go's agentStdoutName): a file the agent writes
// itself, never a pipe into the node, so it outlives the node process
// that started the agent. A hand-off notes how far into it that process
// had got (AgentStdoutOffset); the node that adopts the lease follows it
// from there (followAgentOutput). A lease whose agent never wrote one
// adopts from offset 0.
func (r *Runtime) agentOutputPath(id string) string {
	return filepath.Join(r.leaseDir(id), agentStdoutName)
}

// pastOffset is how many bytes the lease's agent output holds past
// offset: what its agent wrote that the node which wrote the hand-off
// never read. A file that does not exist, or one no longer than offset,
// has nothing new.
func (r *Runtime) pastOffset(id string, offset int64) (int64, error) {
	fi, err := os.Stat(r.agentOutputPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return max(fi.Size()-offset, 0), nil
}

// followAgentOutput is an adopted lease's session showing progress
// (adopt.go): every line its agent writes to its output from offset on
// moves the lease's progress_at, as each event of the session's stream
// did for the node that started it (lease.go's OnEvent), so Mesh's
// stall reaper hears a session a restart left running. It returns once
// ctx ends.
func (r *Runtime) followAgentOutput(ctx context.Context, id string, offset int64) {
	fl, err := sessionunit.Follow(ctx, r.agentOutputPath(id), offset, func() bool { return ctx.Err() == nil })
	if err != nil {
		if !os.IsNotExist(err) {
			r.logf("nodev1: lease %s: following its agent output: %v", id, err)
		}
		return
	}
	defer fl.Close()
	buf := make([]byte, 64<<10)
	for {
		n, err := fl.Read(buf)
		if n > 0 {
			r.touch(id)
		}
		if err != nil {
			return
		}
	}
}

// Upgrade is the node choosing, per live lease, between a hand-off and a
// hand-back, the way a self-update would call it before swapping its
// binary: a lease whose tunnel terminates TLS trusts a CA held only in
// this process's memory (intercepts, with no egress process of its own)
// could never be picked back up by a later process -- it is handed back
// node_update, the same cause every lease the drain's wait outlives gets
// (handBackAll). An intercepting lease whose egress runs as a process of
// its own (ADR-051 decisions 1 and 4) is handed off instead, as long as
// that process is still alive: adoptability names the same condition
// Status reports as Adoptable. Any other lease is left running
// untouched: its brief, sessionproxy socket, turn ledger and push heads
// go to its own hand-off file instead, and nothing about it -- not its
// pod, its sidecars, its agent, nor the lease itself -- is stopped,
// cancelled or reported to Mesh. A lease whose hand-off cannot be
// written is handed back instead, so it is never silently lost. Upgrade
// only starts each hand-back's cancel; it returns the ids it handed back
// so a caller that must not exit before Mesh hears of them
// (UpgradeSignal) can wait on them.
func (r *Runtime) Upgrade(why string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var handedBack []string
	handBack := func(id string, l *running) {
		if l.cancelled == "" {
			l.cancelled = why
		}
		if l.actor != nil {
			l.actor.Cancel(why)
		}
		l.cancel(errNodeUpdate)
		handedBack = append(handedBack, id)
	}
	for _, e := range r.leases.All() {
		id, l := e.ID, e.Value
		if l.intercepts {
			if adoptable, _ := adoptability(l); !adoptable {
				handBack(id, l)
				continue
			}
		}
		if err := r.writeHandoff(id, l); err != nil {
			r.logf("nodev1: lease %s: its hand-off was not written (%v); handed back instead", id, err)
			handBack(id, l)
			continue
		}
		if l.disownEgress != nil {
			l.disownEgress()
		}
	}
	return handedBack
}
