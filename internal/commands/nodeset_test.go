//go:build !windows

package commands

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/meshapi/meshapitest"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/nodev1"
)

// registeredCapacity runs `mesh node run --api v1` end to end against a
// fake Mesh, with node.yaml pre-seeded (or not) and --cpus set to
// cpusFlag, and returns the capacity it registered with — the
// flags-become-defaults rule's observable (ADR-044 amendment,
// 2026-11-03): node.yaml's v1.capacity.cpus wins over the flag when it
// is set, and the flag is the default when node.yaml names none.
func registeredCapacity(t *testing.T, nodeYAML string, cpusFlag float64) meshapi.Capacity {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MESH_TOKEN", "rs_test")
	fake := meshapitest.NewFake(t)
	mesh := httptest.NewServer(fake)
	t.Cleanup(mesh.Close)
	t.Setenv("MESH_API_URL", mesh.URL)

	meshDir := filepath.Join(home, ".mesh")
	if err := os.MkdirAll(meshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if nodeYAML != "" {
		if err := os.WriteFile(filepath.Join(meshDir, "node.yaml"), []byte(nodeYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	saved := []any{nodeRunAPI, nodeRunName, nodeRunWorkRoot, nodeRunOnce, nodeRunCPUs}
	t.Cleanup(func() {
		nodeRunAPI, nodeRunName, nodeRunWorkRoot, nodeRunOnce, nodeRunCPUs =
			saved[0].(string), saved[1].(string), saved[2].(string), saved[3].(bool), saved[4].(float64)
	})
	nodeRunAPI, nodeRunName, nodeRunWorkRoot, nodeRunOnce, nodeRunCPUs =
		"v1", "precedence-test", filepath.Join(home, "work"), false, cpusFlag

	done := make(chan error, 1)
	go func() { done <- nodeRunCmd.RunE(nodeRunCmd, nil) }()
	t.Cleanup(func() {
		if v1, err := v1RunState(); err == nil {
			if pid, ok := liveNode(v1); ok {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})

	deadline := time.Now().Add(20 * time.Second)
	for {
		if calls := fake.Calls("register"); len(calls) > 0 {
			var req meshapi.RegisterRequest
			if err := json.Unmarshal(calls[0].Body, &req); err != nil {
				t.Fatal(err)
			}
			return req.CapacityTotal
		}
		select {
		case err := <-done:
			t.Fatalf("mesh node run exited early: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the node never registered")
		}
	}
}

func TestNodeRunV1CapacityNodeYAMLOverridesTheFlag(t *testing.T) {
	cap := registeredCapacity(t, "v1:\n  capacity:\n    cpus: 64\n", 48)
	if cap.CPUs != 64 {
		t.Fatalf("capacity_total.cpus = %g, want 64 (node.yaml overrides --cpus)", cap.CPUs)
	}
}

func TestNodeRunV1CapacityFlagIsTheDefaultWithNoNodeYAMLCPUs(t *testing.T) {
	cap := registeredCapacity(t, "", 48)
	if cap.CPUs != 48 {
		t.Fatalf("capacity_total.cpus = %g, want 48 (the flag; node.yaml names none)", cap.CPUs)
	}
}

// `mesh node set --cpus 64` rewrites node.yaml and signals the running
// v1 node (SIGHUP): its scheduler's capacity reflects 64 within the
// test's wait, with no restart (ADR-044 amendment, 2026-11-03).
func TestNodeSetChangesCapacityWithoutARestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MESH_TOKEN", "rs_test")
	fake := meshapitest.NewFake(t)
	mesh := httptest.NewServer(fake)
	t.Cleanup(mesh.Close)
	t.Setenv("MESH_API_URL", mesh.URL)
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o700); err != nil {
		t.Fatal(err)
	}

	saved := []any{nodeRunAPI, nodeRunName, nodeRunWorkRoot, nodeRunOnce, nodeRunCPUs}
	t.Cleanup(func() {
		nodeRunAPI, nodeRunName, nodeRunWorkRoot, nodeRunOnce, nodeRunCPUs =
			saved[0].(string), saved[1].(string), saved[2].(string), saved[3].(bool), saved[4].(float64)
	})
	nodeRunAPI, nodeRunName, nodeRunWorkRoot, nodeRunOnce, nodeRunCPUs =
		"v1", "set-test", filepath.Join(home, "work"), false, 4

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	capacityLines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if line := sc.Text(); strings.Contains(line, "nodev1: capacity:") {
				capacityLines <- line
			}
		}
	}()
	t.Cleanup(func() {
		_ = w.Close()
		_ = r.Close()
	})

	done := make(chan error, 1)
	go func() { done <- nodeRunCmd.RunE(nodeRunCmd, nil) }()
	t.Cleanup(func() {
		if v1, err := v1RunState(); err == nil {
			if pid, ok := liveNode(v1); ok {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})

	v1, err := v1RunState()
	if err != nil {
		t.Fatal(err)
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
	// logger has already captured the pipe's *os.File by now; safe to
	// give stderr back to the test process.
	os.Stderr = oldStderr

	pid, ok := liveNode(v1)
	if !ok {
		t.Fatal("no live v1 node")
	}
	if pid != os.Getpid() {
		t.Fatalf("pid %d, want this test process's own %d", pid, os.Getpid())
	}

	if err := nodeSetCmd.Flags().Set("cpus", "64"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		nodeSetCPUs = 0
		nodeSetCmd.Flags().Lookup("cpus").Changed = false
	})
	var out strings.Builder
	nodeSetCmd.SetOut(&out)
	if err := nodeSetCmd.RunE(nodeSetCmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "signalled") {
		t.Fatalf("mesh node set printed %q, want it to say it signalled the node", out.String())
	}

	path, err := node.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := nodev1.ReadCapacityConfig(path)
	if err != nil || !ok {
		t.Fatalf("node.yaml capacity: ok=%v err=%v", ok, err)
	}
	if got.CPUs != 64 {
		t.Fatalf("node.yaml v1.capacity.cpus = %g, want 64", got.CPUs)
	}

	select {
	case line := <-capacityLines:
		if !strings.Contains(line, "-> 64") {
			t.Fatalf("capacity log line %q does not name the new total", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the running node never reread its capacity after mesh node set signalled it")
	}

	pid2, ok2 := liveNode(v1)
	if !ok2 || pid2 != pid {
		t.Fatalf("pid %d -> %d (ok %v): the node restarted", pid, pid2, ok2)
	}
	select {
	case err := <-done:
		t.Fatalf("mesh node run exited (%v); it should still be running", err)
	default:
	}
}
