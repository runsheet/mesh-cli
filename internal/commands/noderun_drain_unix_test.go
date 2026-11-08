//go:build !windows

package commands

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi/meshapitest"
	"projectmesh.io/mesh-cli/internal/node"
)

// `mesh node run` end to end in this process, against the
// fake Mesh: it leaves its pidfile and status in ~/.mesh/node-v1,
// `mesh node drain` signals it SIGUSR1 with a wait, the node tells Mesh
// it drains, takes nothing more, and the command returns nil — the
// process's exit 0 — leaving no run state behind.
func TestNodeRunV1DrainsOnTheDrainCommandAndExitsZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MESH_TOKEN", "rs_test")
	fake := meshapitest.NewFake(t)
	fake.TakeWait = time.Minute
	mesh := httptest.NewServer(fake)
	t.Cleanup(mesh.Close)
	t.Setenv("MESH_API_URL", mesh.URL)
	saved := []any{nodeRunName, nodeRunWorkRoot, nodeRunOnce}
	t.Cleanup(func() {
		nodeRunName, nodeRunWorkRoot, nodeRunOnce = saved[0].(string), saved[1].(string), saved[2].(bool)
	})
	nodeRunName, nodeRunWorkRoot, nodeRunOnce = "drain-test", filepath.Join(home, "work"), false

	done := make(chan error, 1)
	go func() { done <- nodeRunCmd.RunE(nodeRunCmd, nil) }()
	v1, err := v1RunState()
	if err != nil {
		t.Fatal(err)
	}
	if v1.Dir != filepath.Join(home, ".mesh", "node-v1") {
		t.Fatalf("v1 run state in %s", v1.Dir)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, ok := readV1Status(v1); ok && len(fake.Calls("take")) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node never registered and took")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid, ok := liveNode(v1); !ok || pid != os.Getpid() {
		t.Fatalf("pidfile %d, %v", pid, ok)
	}
	if lines, ok := v1StatusLines(v1, time.Now()); !ok || !strings.Contains(strings.Join(lines, "\n"), "Mesh v1 node running") {
		t.Fatalf("status lines %v, %v", lines, ok)
	}

	var out strings.Builder
	if err := drainEither(v1, 10*time.Minute, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "after 10m0s is handed back node_update") {
		t.Fatalf("drain printed %q", out.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("mesh node run = %v, want nil (exit 0)", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the drained node never exited")
	}
	if calls := fake.Calls("drain"); len(calls) != 1 || !strings.Contains(string(calls[0].Body), `"drain":true`) {
		t.Fatalf("drain calls %+v", calls)
	}
	if !fake.Draining() {
		t.Fatal("Mesh does not hold the node drained")
	}
	if _, err := os.Stat(filepath.Join(home, "work", "mesh-drained")); err != nil {
		t.Fatalf("no drain marker for the next start: %v", err)
	}
	takes := len(fake.Calls("take"))
	time.Sleep(100 * time.Millisecond)
	if n := len(fake.Calls("take")); n != takes {
		t.Fatalf("takes went on after the exit: %d, then %d", takes, n)
	}
	for _, p := range []string{v1.PIDPath(), v1.DrainingPath(), v1StatusPath(v1), v1DrainWaitPath(v1)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s outlived the node: %v", p, err)
		}
	}
}

// The wait a drain asked for is read once, as the signal arrives.
func TestTheDrainWaitIsReadOnce(t *testing.T) {
	v1 := node.RunState{Dir: t.TempDir()}
	if err := os.WriteFile(v1DrainWaitPath(v1), []byte("15m0s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := takeV1DrainWait(v1); d != 15*time.Minute {
		t.Fatalf("wait %s", d)
	}
	if d := takeV1DrainWait(v1); d != 0 {
		t.Fatalf("wait read twice: %s", d)
	}
}

// With no v1 node running but a legacy node (`mesh node start`) live,
// drain, status and upgrade all say so clearly instead of reading as
// if nothing runs on the machine (adr-044's amendment, the --api v0
// path is gone): drain refuses naming the legacy pid, status adds a
// line naming it beside the v1 "not running" line, and upgrade (whose
// binary swap already happened) reports it rather than claiming the
// next `mesh node run` will pick up the new binary.
func TestOnlyALegacyNodeRunningIsNamedNotMistakenForNothingRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	legacy, err := node.DefaultRunState()
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Started(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	v1, err := v1RunState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := liveNode(v1); ok {
		t.Fatal("no v1 node was started")
	}

	// drain refuses clearly, naming the legacy node's pid, rather than
	// drainV1Node's "no Mesh v1 node is running".
	var drainOut strings.Builder
	err = drainEither(v1, 0, &drainOut)
	if err == nil {
		t.Fatal("drainEither = nil, want a refusal naming the legacy node")
	}
	if !strings.Contains(err.Error(), "legacy node") || !strings.Contains(err.Error(), "mesh node start") {
		t.Fatalf("drainEither = %v, want it to name the legacy node", err)
	}
	wantPID := strconv.Itoa(os.Getpid())
	if !strings.Contains(err.Error(), wantPID) {
		t.Fatalf("drainEither = %v, want pid %s", err, wantPID)
	}

	// status still says the v1 node is not running, but no longer only
	// that: it also names the legacy node running instead.
	lines, ok := v1StatusLines(v1, time.Now())
	if !ok {
		t.Fatal("v1StatusLines ok = false")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "no Mesh v1 node running") {
		t.Fatalf("status lines %v, want the v1 \"not running\" line kept", lines)
	}
	if !strings.Contains(joined, "legacy node") || !strings.Contains(joined, wantPID) {
		t.Fatalf("status lines %v, want a line naming the running legacy node", lines)
	}

	// upgrade's restart step (the binary swap already done) reports the
	// running legacy node rather than claiming the next `mesh node run`
	// will carry the new binary.
	var upgradeOut strings.Builder
	if err := (v1Service{}).Restart(context.Background(), v1, &upgradeOut); err != nil {
		t.Fatalf("v1Service.Restart = %v, want nil (the swap already succeeded)", err)
	}
	if !strings.Contains(upgradeOut.String(), "legacy node") || !strings.Contains(upgradeOut.String(), wantPID) {
		t.Fatalf("upgrade restart printed %q, want it to name the running legacy node", upgradeOut.String())
	}
}
