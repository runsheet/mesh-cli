package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/nodev1"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// runNodeRunUpToBuild runs nodeRunCmd.RunE far enough to build its
// *nodev1.Runtime -- through every local, no-network step up to the
// --sandbox/--relay-sidecar decision -- and returns it, short-circuiting
// before Run ever reaches the (here, fake) network: RunE builds the
// Runtime as a local variable and never otherwise exposes it
// (nodeupgrade_v1_unix_test.go notes the same).
func runNodeRunUpToBuild(t *testing.T) (*nodev1.Runtime, error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MESH_TOKEN", "rs_test")

	saved := nodeRunAPI
	nodeRunAPI = "v1"
	t.Cleanup(func() { nodeRunAPI = saved })
	savedRoot := nodeRunWorkRoot
	nodeRunWorkRoot = filepath.Join(home, "work")
	t.Cleanup(func() { nodeRunWorkRoot = savedRoot })
	savedActors := nodeRunActors
	nodeRunActors = true
	t.Cleanup(func() { nodeRunActors = savedActors })

	savedRT := nodeRunSandboxRuntime
	nodeRunSandboxRuntime = &sandbox.Fake{}
	t.Cleanup(func() { nodeRunSandboxRuntime = savedRT })

	var built *nodev1.Runtime
	savedHook := nodeRunBeforeRun
	nodeRunBeforeRun = func(rt *nodev1.Runtime) bool {
		built = rt
		return true
	}
	t.Cleanup(func() { nodeRunBeforeRun = savedHook })

	err := nodeRunCmd.RunE(nodeRunCmd, nil)
	return built, err
}

// --sandbox with no --relay-sidecar flag defaults the relay to a
// sidecar in the lease's pod: the one shape adoption can pick back up
// across a restart (adopt.go's "its hand-off named no relay sidecar to
// adopt").
func TestNodeRunSandboxDefaultsRelaySidecarOn(t *testing.T) {
	nodeRunCmd.Flags().Lookup("relay-sidecar").Changed = false
	savedSandbox := nodeRunSandbox
	nodeRunSandbox = true
	t.Cleanup(func() { nodeRunSandbox = savedSandbox })
	savedRelay := nodeRunRelaySidecar
	nodeRunRelaySidecar = false
	t.Cleanup(func() { nodeRunRelaySidecar = savedRelay })

	rt, err := runNodeRunUpToBuild(t)
	if err != nil {
		t.Fatalf("mesh node run --sandbox: %v", err)
	}
	if rt == nil {
		t.Fatal("the hook was never called; RunE never reached the --sandbox block")
	}
	if !rt.RelaySidecar {
		t.Fatal("RelaySidecar = false, want true (the default with --sandbox)")
	}
}

// --sandbox --relay-sidecar=false is honoured, never forced on.
func TestNodeRunSandboxRelaySidecarExplicitFalseIsHonoured(t *testing.T) {
	savedSandbox := nodeRunSandbox
	nodeRunSandbox = true
	t.Cleanup(func() { nodeRunSandbox = savedSandbox })
	if err := nodeRunCmd.Flags().Set("relay-sidecar", "false"); err != nil {
		t.Fatal(err)
	}
	savedRelay := nodeRunRelaySidecar
	t.Cleanup(func() {
		nodeRunRelaySidecar = savedRelay
		nodeRunCmd.Flags().Lookup("relay-sidecar").Changed = false
	})

	rt, err := runNodeRunUpToBuild(t)
	if err != nil {
		t.Fatalf("mesh node run --sandbox --relay-sidecar=false: %v", err)
	}
	if rt == nil {
		t.Fatal("the hook was never called; RunE never reached the --sandbox block")
	}
	if rt.RelaySidecar {
		t.Fatal("RelaySidecar = true, want false (explicitly asked for)")
	}
}

// --relay-sidecar with no --sandbox still errors, unchanged.
func TestNodeRunRelaySidecarWithoutSandboxStillErrors(t *testing.T) {
	savedSandbox := nodeRunSandbox
	nodeRunSandbox = false
	t.Cleanup(func() { nodeRunSandbox = savedSandbox })
	savedRelay := nodeRunRelaySidecar
	nodeRunRelaySidecar = true
	t.Cleanup(func() { nodeRunRelaySidecar = savedRelay })

	rt, err := runNodeRunUpToBuild(t)
	if err == nil {
		t.Fatal("--relay-sidecar with no --sandbox: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "--relay-sidecar runs each lease's relay in its pod: it needs --sandbox") {
		t.Fatalf("error = %v, want it to name --sandbox", err)
	}
	if rt != nil {
		t.Fatal("the hook was called; RunE should have returned before the --sandbox block")
	}
}

// mesh node run wires the live build-cache watcher by default -- no
// flag needed -- at the same node.yaml path it already resolves for
// capacity, retiring the exclusive --cache-trim (goal
// a-nodes-build-cache-stays-under-its-size).
func TestNodeRunWiresBuildCacheWatcherByDefault(t *testing.T) {
	rt, err := runNodeRunUpToBuild(t)
	if err != nil {
		t.Fatalf("mesh node run: %v", err)
	}
	if rt == nil {
		t.Fatal("the hook was never called; RunE never built the Runtime")
	}
	want, err := node.ConfigPath()
	if err != nil {
		t.Fatalf("node.ConfigPath: %v", err)
	}
	if rt.BuildCacheConfigPath != want {
		t.Fatalf("BuildCacheConfigPath = %q, want %q (the same node.yaml capacity resolves)", rt.BuildCacheConfigPath, want)
	}
	if rt.BuildCacheReload == nil {
		t.Fatal("BuildCacheReload = nil, want a channel so mesh node set wakes the watcher")
	}
	for _, name := range []string{"cache-trim", "cache-dir", "cache-max-gb"} {
		if f := nodeRunCmd.Flags().Lookup(name); f != nil {
			t.Fatalf("flag %q is still registered; the exclusive --cache-trim should be retired", name)
		}
	}
}
