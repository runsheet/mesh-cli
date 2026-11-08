package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/nodev1"
	"projectmesh.io/mesh-cli/internal/paths"
)

// A Mesh v1 node (`mesh node run`) leaves its run state beside
// the legacy node's, in ~/.mesh/node-v1 (ADR-044 amendment, drain): its
// pidfile (`mesh node drain` signals it SIGUSR1), its draining marker,
// the wait a drain asked for (node.drain-wait, read by the node as the
// signal arrives) and its status (node.status, which `mesh node status`
// shows). Its own directory keeps a v1 node and a legacy node run by one
// user apart.

// v1RunStateDir is the v1 node's run state directory under ~/.mesh.
const v1RunStateDir = "node-v1"

// v1RunState is the v1 node's run state: ~/.mesh/node-v1.
func v1RunState() (node.RunState, error) {
	dir, err := paths.MeshDir()
	if err != nil {
		return node.RunState{}, err
	}
	return node.RunState{Dir: filepath.Join(dir, v1RunStateDir)}, nil
}

// v1DrainWaitPath is the wait the next drain signal carries.
func v1DrainWaitPath(s node.RunState) string { return filepath.Join(s.Dir, "node.drain-wait") }

// v1StatusPath is the v1 node's status.
func v1StatusPath(s node.RunState) string { return filepath.Join(s.Dir, "node.status") }

// v1UpgradeAskPath marks that the next SIGHUP is `mesh node upgrade`'s,
// not `mesh node set`'s: both signal the same running v1 node with
// SIGHUP (adr-044, both amendments), so `v1Service.Restart` writes this
// right before it signals and the restart handler reads and clears it
// (takeV1UpgradeAsk) to tell which was meant.
func v1UpgradeAskPath(s node.RunState) string { return filepath.Join(s.Dir, "node.upgrade-ask") }

// v1Started records pid as the running v1 node, clearing what a node
// that died left.
func v1Started(s node.RunState, pid int) error {
	_ = os.Remove(v1DrainWaitPath(s))
	_ = os.Remove(v1StatusPath(s))
	_ = os.Remove(v1UpgradeAskPath(s))
	return s.Started(pid)
}

// v1Stopped removes the v1 node's run state as it exits.
func v1Stopped(s node.RunState, pid int) {
	s.Stopped(pid)
	_ = os.Remove(v1DrainWaitPath(s))
	_ = os.Remove(v1StatusPath(s))
	_ = os.Remove(v1UpgradeAskPath(s))
}

// writeV1Status leaves the node's status for `mesh node status`.
func writeV1Status(s node.RunState, st nodev1.Status) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := v1StatusPath(s) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, v1StatusPath(s))
}

// readV1Status is the status the running v1 node left; false when none
// is there.
func readV1Status(s node.RunState) (nodev1.Status, bool) {
	data, err := os.ReadFile(v1StatusPath(s))
	if err != nil {
		return nodev1.Status{}, false
	}
	var st nodev1.Status
	if json.Unmarshal(data, &st) != nil {
		return nodev1.Status{}, false
	}
	return st, true
}

// takeV1DrainWait reads and removes the wait a drain asked for; 0 when
// it asked none.
func takeV1DrainWait(s node.RunState) time.Duration {
	data, err := os.ReadFile(v1DrainWaitPath(s))
	if err != nil {
		return 0
	}
	_ = os.Remove(v1DrainWaitPath(s))
	d, err := time.ParseDuration(strings.TrimSpace(string(data)))
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// writeV1UpgradeAsk marks that the SIGHUP about to be sent is `mesh node
// upgrade`'s, written right before the signal (the drain wait's own
// precedent).
func writeV1UpgradeAsk(s node.RunState) error {
	return os.WriteFile(v1UpgradeAskPath(s), []byte("1\n"), 0o600)
}

// takeV1UpgradeAsk reads and removes the upgrade marker; true when the
// SIGHUP that just arrived was `mesh node upgrade`'s, false when it was
// `mesh node set`'s reload.
func takeV1UpgradeAsk(s node.RunState) bool {
	_, err := os.Stat(v1UpgradeAskPath(s))
	asked := err == nil
	_ = os.Remove(v1UpgradeAskPath(s))
	return asked
}

// liveNode is the pid in state's pidfile when that process runs.
func liveNode(s node.RunState) (int, bool) {
	pid, err := s.PID()
	if err != nil || !processAlive(pid) {
		return 0, false
	}
	return pid, true
}

// drainV1Node signals the v1 node whose pid state holds to drain, with
// wait (0 for none) after which it hands back what still runs.
func drainV1Node(s node.RunState, wait time.Duration, out io.Writer) error {
	pid, err := s.PID()
	if errors.Is(err, node.ErrNoNode) {
		return fmt.Errorf("no Mesh v1 node is running on this machine (no %s)", s.PIDPath())
	}
	if err != nil {
		return err
	}
	if !processAlive(pid) {
		return fmt.Errorf("no Mesh v1 node is running on this machine: pid %d in %s is gone", pid, s.PIDPath())
	}
	if wait > 0 {
		if err := os.WriteFile(v1DrainWaitPath(s), []byte(wait.String()+"\n"), 0o600); err != nil {
			return fmt.Errorf("the drain's wait: %w", err)
		}
	} else {
		_ = os.Remove(v1DrainWaitPath(s))
	}
	if err := signalDrain(pid); err != nil {
		return fmt.Errorf("signal node (pid %d): %w", pid, err)
	}
	msg := fmt.Sprintf("Mesh v1 node (pid %d) is draining: it takes no new lease, tells Mesh, and exits 0 once its leases have ended", pid)
	if wait > 0 {
		msg += fmt.Sprintf("; what still runs after %s is handed back node_update", wait)
	}
	fmt.Fprintln(out, msg+".")
	return nil
}

// v1StatusLines are `mesh node status`'s lines for the v1 node on this
// machine: what it is doing when one runs, else that none does — always
// at least one line, so `mesh node status` never falls back to legacy
// text for this machine.
func v1StatusLines(s node.RunState, now time.Time) ([]string, bool) {
	pid, ok := liveNode(s)
	if !ok {
		return []string{"This machine: no Mesh v1 node running (mesh node run)."}, true
	}
	st, ok := readV1Status(s)
	if !ok || st.PID != pid {
		line := fmt.Sprintf("This machine: Mesh v1 node running (pid %d), not yet registered.", pid)
		if s.Draining() {
			line = fmt.Sprintf("This machine: Mesh v1 node running (pid %d), draining.", pid)
		}
		return []string{line}, true
	}
	return st.DrainLines(now), true
}
