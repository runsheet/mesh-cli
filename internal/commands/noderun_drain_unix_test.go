//go:build !windows

package commands

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi/meshapitest"
	"projectmesh.io/mesh-cli/internal/node"
)

// `mesh node run --api v1` end to end in this process, against the
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
	saved := []any{nodeRunAPI, nodeRunName, nodeRunWorkRoot, nodeRunOnce}
	t.Cleanup(func() {
		nodeRunAPI, nodeRunName, nodeRunWorkRoot, nodeRunOnce = saved[0].(string), saved[1].(string), saved[2].(string), saved[3].(bool)
	})
	nodeRunAPI, nodeRunName, nodeRunWorkRoot, nodeRunOnce = "v1", "drain-test", filepath.Join(home, "work"), false

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

	legacy, _ := node.DefaultRunState()
	var out strings.Builder
	if err := drainEither(legacy, v1, "", 10*time.Minute, &out); err != nil {
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

// drain picks the node: --api names it; with none, the one running; a
// legacy and a v1 node both running need --api; --wait is the v1
// node's alone.
func TestDrainEitherPicksTheNode(t *testing.T) {
	legacy := node.RunState{Dir: t.TempDir()}
	v1 := node.RunState{Dir: t.TempDir()}
	if err := drainEither(legacy, v1, "v2", 0, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "v0 or v1") {
		t.Fatalf("--api v2: %v", err)
	}
	if err := drainEither(legacy, v1, "v1", 0, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "no Mesh v1 node") {
		t.Fatalf("no v1 node: %v", err)
	}
	if err := drainEither(legacy, v1, "", time.Minute, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "--wait is the Mesh v1 node's") {
		t.Fatalf("--wait on the legacy node: %v", err)
	}
	pid := os.Getpid()
	if err := legacy.Started(pid); err != nil {
		t.Fatal(err)
	}
	if err := v1Started(v1, pid); err != nil {
		t.Fatal(err)
	}
	if err := drainEither(legacy, v1, "", 0, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "both run") {
		t.Fatalf("both running: %v", err)
	}
	if err := drainEither(legacy, v1, "", -time.Second, &strings.Builder{}); err == nil {
		t.Fatal("a negative wait was taken")
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
