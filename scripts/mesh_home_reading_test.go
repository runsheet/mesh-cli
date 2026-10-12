// Tests scripts/mesh-home-reading.sh against the fixtures under
// scripts/testdata/mesh-home/ with PODMAN pointed at a fake podman
// script and MESH_HOME_ROOT pointed at a fixture directory, so the
// test never touches a real podman or the user's own /tmp: a clean
// fixture (every mesh-home dir mounted, every pod running) and a
// dirty one (one dir not mounted, one pod with nothing running, one
// exited container) — ADR-020, this is a `Measure:` item's reading,
// never a verdict on a build.
package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runMeshHomeReading(t *testing.T, fixture string) string {
	t.Helper()
	dir, err := filepath.Abs("testdata/mesh-home/" + fixture)
	if err != nil {
		t.Fatalf("abs path for fixture %s: %v", fixture, err)
	}
	cmd := exec.Command("bash", "mesh-home-reading.sh")
	cmd.Env = append(os.Environ(),
		"PODMAN="+filepath.Join(dir, "podman"),
		"MESH_HOME_ROOT="+filepath.Join(dir, "home"),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("mesh-home-reading.sh %s: %v\noutput:\n%s", fixture, err, out.String())
	}
	return out.String()
}

func TestMeshHomeReadingSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", "mesh-home-reading.sh")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("bash -n mesh-home-reading.sh: %v\n%s", err, out.String())
	}
}

func TestMeshHomeReadingHelp(t *testing.T) {
	cmd := exec.Command("bash", "mesh-home-reading.sh", "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "usage: scripts/mesh-home-reading.sh") {
		t.Fatalf("--help printed no usage line:\n%s", out)
	}
}

func TestMeshHomeReadingClean(t *testing.T) {
	out := runMeshHomeReading(t, "clean")

	if !containsLine(out, "ok") {
		t.Fatalf("clean fixture: expected a bare \"ok\" line, got:\n%s", out)
	}
	// A line, not a substring: the checkout's own path may say "stale".
	if containsLine(out, "stale") || strings.Contains(out, "reason=") {
		t.Fatalf("clean fixture: expected no \"stale\" line, got:\n%s", out)
	}
	if !strings.Contains(out, "mesh-home-aaa\tbytes=") || !strings.Contains(out, "mounted=yes") {
		t.Fatalf("clean fixture: expected mesh-home-aaa reported as mounted, got:\n%s", out)
	}
	if !containsLine(out, "mesh-abc\tstatus=Running") {
		t.Fatalf("clean fixture: expected mesh-abc reported as Running, got:\n%s", out)
	}
}

func TestMeshHomeReadingDirty(t *testing.T) {
	out := runMeshHomeReading(t, "dirty")

	if !containsLine(out, "stale") {
		t.Fatalf("dirty fixture: expected a \"stale\" line, got:\n%s", out)
	}
	if !strings.Contains(out, "mesh-home-bbb\treason=not mounted") {
		t.Fatalf("dirty fixture: expected mesh-home-bbb named as not mounted, got:\n%s", out)
	}
	if !strings.Contains(out, "mesh-def\treason=not running") {
		t.Fatalf("dirty fixture: expected mesh-def named as not running, got:\n%s", out)
	}
	if !strings.Contains(out, "mesh-home-aaa\tbytes=") || !strings.Contains(out, "mounted=yes") {
		t.Fatalf("dirty fixture: expected mesh-home-aaa reported as mounted, got:\n%s", out)
	}
	if !strings.Contains(out, "mesh-def-agent\tpod=pod-def\tstatus=exited") {
		t.Fatalf("dirty fixture: expected the exited container named, got:\n%s", out)
	}
}

func TestMeshHomeReadingPodmanMissing(t *testing.T) {
	dir, err := filepath.Abs("testdata/mesh-home/dirty")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	cmd := exec.Command("bash", "mesh-home-reading.sh")
	cmd.Env = append(os.Environ(),
		"PODMAN=/nonexistent/podman",
		"MESH_HOME_ROOT="+filepath.Join(dir, "home"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("podman missing: exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "podman: /nonexistent/podman not found") {
		t.Fatalf("podman missing: expected a note naming it, got:\n%s", out)
	}
	if !containsLine(string(out), "stale") {
		t.Fatalf("podman missing: dirs still unmounted, expected \"stale\", got:\n%s", out)
	}
}

func containsLine(out, line string) bool {
	for _, l := range strings.Split(out, "\n") {
		if l == line {
			return true
		}
	}
	return false
}
