package commands

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"projectmesh.io/mesh-cli/internal/nodev1"
)

// nodeRunHost reads node.yaml's host: block into nodev1.Host and a
// MachineProbe, since nodev1 may not import internal/node itself
// (guard_test.go) — "A node backs off a hot host" for --api v1.
func TestNodeRunHostReadsNodeYAMLHostBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "host:\n  load_per_core_max: 2.5\n  min_free_memory: \"32g\"\n"
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	host, machine := nodeRunHost(log.New(&logs, "", 0))

	want := nodev1.Host{LoadPerCoreMax: 2.5, MinFreeMemory: 32 << 30}
	if host != want {
		t.Fatalf("Host = %+v, want %+v (log: %s)", host, want, logs.String())
	}
	if runtime.GOOS == "linux" && machine == nil {
		t.Fatal("Machine is nil on linux, where a /proc probe is available")
	}
}

// With no host: block in node.yaml, Host is zero; the probe is still
// set whenever the platform has one, since the gate does nothing with
// zero thresholds.
func TestNodeRunHostWithNoHostBlockIsZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte("slug: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	host, machine := nodeRunHost(log.New(&logs, "", 0))

	if host != (nodev1.Host{}) {
		t.Fatalf("Host = %+v, want zero", host)
	}
	if runtime.GOOS == "linux" && machine == nil {
		t.Fatal("Machine is nil on linux, where a /proc probe is available")
	}
}

// A failed node.yaml load (unparseable) is also a zero Host; the
// command still starts.
func TestNodeRunHostWithFailedLoadIsZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte("host: [this is not a mapping"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	host, _ := nodeRunHost(log.New(&logs, "", 0))

	if host != (nodev1.Host{}) {
		t.Fatalf("Host = %+v, want zero on a failed load", host)
	}
	if logs.Len() == 0 {
		t.Error("a failed node.yaml load was not logged")
	}
}

// With no node.yaml at all, Host is zero too.
func TestNodeRunHostWithNoNodeYAMLIsZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var logs bytes.Buffer
	host, _ := nodeRunHost(log.New(&logs, "", 0))

	if host != (nodev1.Host{}) {
		t.Fatalf("Host = %+v, want zero with no node.yaml", host)
	}
}
