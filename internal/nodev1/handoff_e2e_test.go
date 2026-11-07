//go:build e2e

package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// TestAnUpgradeHandsBackInterceptingLeasesAndAdoptsTheRestNextStart is
// Upgrade and start-up adoption end to end, on one node.log (adr-044,
// the upgrade amendment): a lease whose tunnel terminates TLS on a CA
// this process holds only in memory can never be picked back up by a
// hand-off, so UpgradeSignal hands it back node_update -- never
// node_restart, the cause a drain's own wait uses -- and waits for
// Mesh to hear of it before exiting (handoff.go's Upgrade,
// upgrade_signal.go). A lease with a relay sidecar is left running
// untouched, its hand-off written instead; the node that starts next,
// on the same WorkRoot, picks it back up (adopt.go's adoptHandoffs):
// a fresh session, its sidecar's credentials replaced exactly once,
// its room in the scheduler re-taken, its status reading running and
// heartbeated (renewed) before its lease's deadline, and it runs on
// to finish -- never handed back node_restart itself.
func TestAnUpgradeHandsBackInterceptingLeasesAndAdoptsTheRestNextStart(t *testing.T) {
	var logMu sync.Mutex
	var logs []string
	record := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		logMu.Lock()
		logs = append(logs, msg)
		logMu.Unlock()
		t.Log(msg)
	}
	hasLog := func(substr string) bool {
		logMu.Lock()
		defer logMu.Unlock()
		for _, l := range logs {
			if strings.Contains(l, substr) {
				return true
			}
		}
		return false
	}
	countLog := func(exact string) int {
		logMu.Lock()
		defer logMu.Unlock()
		n := 0
		for _, l := range logs {
			if l == exact {
				n++
			}
		}
		return n
	}

	// --- an intercepting lease is handed back node_update, never
	// node_restart, on the upgrade signal; the process does not exit
	// until Mesh has heard of the hand-back.
	sess := &stubSession{hold: make(chan struct{})}
	g, sb := actorRig(t, sess)
	g.rt.Logf = record
	g.rt.Pods = &sandbox.Sandbox{Runtime: sb, Logf: t.Logf, Config: sandbox.Config{
		Image:          "mesh-node:test",
		Limits:         sandbox.Limits{CPUs: 2, Memory: "6g", Pids: 4096},
		ServicesLimits: sandbox.Limits{CPUs: 2, Memory: "4g"},
		Registries:     sandbox.DefaultRegistries,
		ServiceTimeout: 5 * time.Second,
	}}
	registryHostRig(t, g)
	interceptingJob := g.offer(t, registryBrief)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	waitFor(t, "the intercepting lease's session", func() bool { return len(sess.specsSoFar()) == 1 })

	var exitMu sync.Mutex
	var exits int
	orig := UpgradeExit
	UpgradeExit = func(code int) { exitMu.Lock(); exits++; exitMu.Unlock() }
	defer func() { UpgradeExit = orig }()

	g.rt.UpgradeSignal("the node is updating")

	exitMu.Lock()
	n := exits
	exitMu.Unlock()
	if n != 1 {
		t.Fatalf("UpgradeExit calls %d, want exactly 1", n)
	}

	hb := onlyHandBack(t, g, interceptingJob)
	if hb.Cause != meshapi.CauseNodeUpdate {
		t.Fatalf("the intercepting lease's hand-back cause %q, want %q", hb.Cause, meshapi.CauseNodeUpdate)
	}
	assertReleased(t, g, interceptingJob)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil once the intercepting lease's hand-back was reported", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not end once the intercepting lease was handed back")
	}

	// --- a sidecar-mode lease's hand-off, left behind by the same kind
	// of upgrade, is adopted at the next start: the same Runtime, the
	// way the real process would be a fresh one reading the same
	// WorkRoot, picking the hand-off back up by calling adoptHandoffs
	// directly (adopt_test.go's own TestNodeAdoptsHandoffAtStart does
	// the same, never through Run's take loop).
	leaseID, brief := mintLease(t, g, func(b *meshapi.Brief) {
		b.FinishTool = "submit"
		// a short renew interval so the adopted lease's heartbeat is
		// observable inside waitFor's 10-second budget: renew() falls
		// back to 30s when RenewEveryS is left at its zero default,
		// which would never fire in time.
		b.RenewEveryS = 1
	})

	sidecarDir := filepath.Join(t.TempDir(), "sidecar")
	sidecar := startFakeSidecarControl(t, sidecarDir)

	writeHandoffFixture(t, g, leaseID, &handoffState{Brief: brief, SidecarDir: sidecarDir})

	adoptCtx, adoptCancel := context.WithCancel(context.Background())
	defer adoptCancel()
	g.rt.adoptHandoffs(adoptCtx)

	nodeSocket := filepath.Join(sidecarDir, relayNodeSocket)
	waitFor(t, "the adopted lease's node socket", func() bool {
		_, err := os.Stat(nodeSocket)
		return err == nil
	})

	// adoptOne's own actor never re-enters running -- it moves straight
	// from taken to reporting at the end (actor.go's next map allows
	// Taken: {Preparing, Reporting} exactly so adoption can skip the
	// steps it never redoes) -- so the lease's running status is read
	// from the node's own status report, which holds it from the
	// moment adoptOne registers it in r.leases until it is reported.
	waitFor(t, "the adopted lease to show running in the node's status", func() bool {
		for _, l := range g.rt.Status().Leases {
			if l.LeaseID == leaseID {
				return true
			}
		}
		return false
	})

	waitFor(t, "the adopted lease to be renewed (heartbeated) before its deadline", func() bool {
		for _, c := range g.fake.Calls("renew") {
			if c.LeaseID == leaseID {
				return true
			}
		}
		return false
	})

	if calls := sidecar.replacements(); len(calls) != 1 {
		t.Fatalf("expected exactly one credential replacement for the adopted lease's sidecar, got %d", len(calls))
	}

	driveObserve(t, nodeSocket, "conform", `{"id":1,"method":"tools/call","params":{"name":"submit"}}`,
		`{"id":1,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`)

	waitFor(t, "the adopted lease to finish", func() bool {
		return len(g.fake.Calls("finish")) > 0
	})
	if g.fake.Ended(leaseID) != "finish" {
		t.Fatalf("the adopted lease ended %q, not finish", g.fake.Ended(leaseID))
	}

	if n := countLog("nodev1: adopted 1 of 1 hand-off(s) at start"); n != 1 {
		t.Fatalf("%d occurrence(s) of the aggregate adopt line, want exactly 1; logs: %v", n, logs)
	}
	if hasLog("node_restart") {
		t.Fatal("node.log mentions node_restart: an upgrade must never hand a lease back that way")
	}
	for _, c := range g.fake.Calls("hand-back") {
		var hb meshapi.HandBackRequest
		if err := json.Unmarshal(c.Body, &hb); err == nil && hb.Cause == meshapi.CauseNodeRestart {
			t.Fatalf("a hand-back carried cause %q", meshapi.CauseNodeRestart)
		}
	}
}
