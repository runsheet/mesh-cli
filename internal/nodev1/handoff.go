package nodev1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"projectmesh.io/mesh-cli/internal/meshapi"
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
	st := &handoffState{Brief: l.brief, SocketDir: filepath.Dir(l.sock), PushHeads: l.pushHeads}
	if l.mailbox != nil {
		st.Turns = turnsForHandoff(l.mailbox.turns)
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

// Upgrade is the node choosing, per live lease, between a hand-off and a
// hand-back, the way a self-update would call it before swapping its
// binary: a lease whose tunnel terminates TLS trusts a CA this process
// minted and holds nowhere but memory, so a later process could never
// pick it back up -- it is handed back node_update, the same cause every
// lease the drain's wait outlives gets (handBackAll). Any other lease is
// left running untouched: its brief, sessionproxy socket, turn ledger
// and push heads go to its own hand-off file instead, and nothing about
// it -- not its pod, its sidecars, its agent, nor the lease itself -- is
// stopped, cancelled or reported to Mesh. A lease whose hand-off cannot
// be written is handed back instead, so it is never silently lost.
func (r *Runtime) Upgrade(why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	handBack := func(id string, l *running) {
		if l.cancelled == "" {
			l.cancelled = why
		}
		if l.actor != nil {
			l.actor.Cancel(why)
		}
		l.cancel(errNodeUpdate)
	}
	for _, e := range r.leases.All() {
		id, l := e.ID, e.Value
		if l.intercepts {
			handBack(id, l)
			continue
		}
		if err := r.writeHandoff(id, l); err != nil {
			r.logf("nodev1: lease %s: its hand-off was not written (%v); handed back instead", id, err)
			handBack(id, l)
		}
	}
}
